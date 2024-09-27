// Package agent handles the agent's SSH server and system stats collection.
package agent

import (
	"beszel"
	"beszel/internal/entities/container"
	"beszel/internal/entities/system"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/common"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/sensors"

	psutilNet "github.com/shirou/gopsutil/v4/net"
)

type Agent struct {
	addr                string
	pubKey              []byte
	sem                 chan struct{}
	containerStatsMap   map[string]*container.PrevContainerStats
	containerStatsMutex *sync.Mutex
	fsNames             []string
	fsStats             map[string]*system.FsStats
	netInterfaces       map[string]struct{}
	netIoStats          *system.NetIoStats
	dockerClient        *http.Client
	sensorsContext      context.Context
}

func NewAgent(pubKey []byte, addr string) *Agent {
	return &Agent{
		addr:                addr,
		pubKey:              pubKey,
		sem:                 make(chan struct{}, 15),
		containerStatsMap:   make(map[string]*container.PrevContainerStats),
		containerStatsMutex: &sync.Mutex{},
		netIoStats:          &system.NetIoStats{},
		dockerClient:        newDockerClient(),
		sensorsContext:      context.Background(),
	}
}

func (a *Agent) getSystemStats() (system.Info, system.Stats) {
	systemStats := system.Stats{}

	// cpu percent
	cpuPct, err := cpu.Percent(0, false)
	if err != nil {
		slog.Error("Error getting cpu percent", "err", err)
	} else if len(cpuPct) > 0 {
		systemStats.Cpu = twoDecimals(cpuPct[0])
	}

	// memory
	if v, err := mem.VirtualMemory(); err == nil {
		systemStats.Mem = bytesToGigabytes(v.Total)
		systemStats.MemUsed = bytesToGigabytes(v.Used)
		systemStats.MemBuffCache = bytesToGigabytes(v.Total - v.Free - v.Used)
		systemStats.MemPct = twoDecimals(v.UsedPercent)
		systemStats.Swap = bytesToGigabytes(v.SwapTotal)
		systemStats.SwapUsed = bytesToGigabytes(v.SwapTotal - v.SwapFree)
	}

	// disk usage
	for _, stats := range a.fsStats {
		if d, err := disk.Usage(stats.Mountpoint); err == nil {
			stats.DiskTotal = bytesToGigabytes(d.Total)
			stats.DiskUsed = bytesToGigabytes(d.Used)
			if stats.Root {
				systemStats.DiskTotal = bytesToGigabytes(d.Total)
				systemStats.DiskUsed = bytesToGigabytes(d.Used)
				systemStats.DiskPct = twoDecimals(d.UsedPercent)
			}
		} else {
			// reset stats if error (likely unmounted)
			slog.Error("Error getting disk stats", "name", stats.Mountpoint, "err", err)
			stats.DiskTotal = 0
			stats.DiskUsed = 0
			stats.TotalRead = 0
			stats.TotalWrite = 0
		}
	}

	// disk i/o
	if ioCounters, err := disk.IOCounters(a.fsNames...); err == nil {
		for _, d := range ioCounters {
			stats := a.fsStats[d.Name]
			if stats == nil {
				continue
			}
			secondsElapsed := time.Since(stats.Time).Seconds()
			readPerSecond := float64(d.ReadBytes-stats.TotalRead) / secondsElapsed
			writePerSecond := float64(d.WriteBytes-stats.TotalWrite) / secondsElapsed
			stats.Time = time.Now()
			stats.DiskReadPs = bytesToMegabytes(readPerSecond)
			stats.DiskWritePs = bytesToMegabytes(writePerSecond)
			stats.TotalRead = d.ReadBytes
			stats.TotalWrite = d.WriteBytes
			// if root filesystem, update system stats
			if stats.Root {
				systemStats.DiskReadPs = stats.DiskReadPs
				systemStats.DiskWritePs = stats.DiskWritePs
			}
		}
	}

	// network stats
	if netIO, err := psutilNet.IOCounters(true); err == nil {
		secondsElapsed := time.Since(a.netIoStats.Time).Seconds()
		a.netIoStats.Time = time.Now()
		bytesSent := uint64(0)
		bytesRecv := uint64(0)
		// sum all bytes sent and received
		for _, v := range netIO {
			// skip if not in valid network interfaces list
			if _, exists := a.netInterfaces[v.Name]; !exists {
				continue
			}
			bytesSent += v.BytesSent
			bytesRecv += v.BytesRecv
		}
		// add to systemStats
		sentPerSecond := float64(bytesSent-a.netIoStats.BytesSent) / secondsElapsed
		recvPerSecond := float64(bytesRecv-a.netIoStats.BytesRecv) / secondsElapsed
		networkSentPs := bytesToMegabytes(sentPerSecond)
		networkRecvPs := bytesToMegabytes(recvPerSecond)
		// add check for issue (#150) where sent is a massive number
		if networkSentPs > 10_000 || networkRecvPs > 10_000 {
			slog.Warn("Invalid network stats. Resetting.", "sent", networkSentPs, "recv", networkRecvPs)
			for _, v := range netIO {
				if _, exists := a.netInterfaces[v.Name]; !exists {
					continue
				}
				slog.Info(v.Name, "recv", v.BytesRecv, "sent", v.BytesSent)
			}
			// reset network I/O stats
			a.initializeNetIoStats()
		} else {
			systemStats.NetworkSent = networkSentPs
			systemStats.NetworkRecv = networkRecvPs
			// update netIoStats
			a.netIoStats.BytesSent = bytesSent
			a.netIoStats.BytesRecv = bytesRecv
		}
	}

	// temperatures
	if temps, err := sensors.TemperaturesWithContext(a.sensorsContext); err == nil {
		slog.Debug("Temperatures", "data", temps)
		systemStats.Temperatures = make(map[string]float64)
		for i, temp := range temps {
			if _, ok := systemStats.Temperatures[temp.SensorKey]; ok {
				// if key already exists, append int to key
				systemStats.Temperatures[temp.SensorKey+"_"+strconv.Itoa(i)] = twoDecimals(temp.Temperature)
			} else {
				systemStats.Temperatures[temp.SensorKey] = twoDecimals(temp.Temperature)
			}
		}
	} else {
		slog.Debug("Error getting temperatures", "err", err)
	}

	systemInfo := system.Info{
		Cpu:          systemStats.Cpu,
		MemPct:       systemStats.MemPct,
		DiskPct:      systemStats.DiskPct,
		AgentVersion: beszel.Version,
	}

	// add host info
	if info, err := host.Info(); err == nil {
		// slog.Debug("Virtualization", "system", info.VirtualizationSystem, "role", info.VirtualizationRole)
		systemInfo.Uptime = info.Uptime
		systemInfo.Hostname = info.Hostname
		systemInfo.KernelVersion = info.KernelVersion
	}

	// add cpu stats
	if info, err := cpu.Info(); err == nil && len(info) > 0 {
		systemInfo.CpuModel = info[0].ModelName
	}
	if cores, err := cpu.Counts(false); err == nil {
		systemInfo.Cores = cores
	}
	if threads, err := cpu.Counts(true); err == nil {
		if threads > 0 && threads < systemInfo.Cores {
			// in lxc logical cores reflects container limits, so use that as cores if lower
			systemInfo.Cores = threads
		} else {
			systemInfo.Threads = threads
		}
	}

	return systemInfo, systemStats
}

func (a *Agent) getDockerStats() ([]container.Stats, error) {
	resp, err := a.dockerClient.Get("http://localhost/containers/json")
	if err != nil {
		a.closeIdleConnections(err)
		return nil, err
	}
	defer resp.Body.Close()

	var containers []container.ApiInfo
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		slog.Error("Error decoding containers", "err", err)
		return nil, err
	}

	containerStats := make([]container.Stats, 0, len(containers))
	containerStatsMutex := sync.Mutex{}

	// store valid ids to clean up old container ids from map
	validIds := make(map[string]struct{}, len(containers))

	var wg sync.WaitGroup

	for _, ctr := range containers {
		ctr.IdShort = ctr.Id[:12]
		validIds[ctr.IdShort] = struct{}{}
		// check if container is less than 1 minute old (possible restart)
		// note: can't use Created field because it's not updated on restart
		if strings.Contains(ctr.Status, "second") {
			// if so, remove old container data
			a.deleteContainerStatsSync(ctr.IdShort)
		}
		wg.Add(1)
		a.acquireSemaphore()
		go func() {
			defer a.releaseSemaphore()
			defer wg.Done()
			cstats, err := a.getContainerStats(ctr)
			if err != nil {
				// close idle connections if error is a network timeout
				isTimeout := a.closeIdleConnections(err)
				// delete container from map if not a timeout
				if !isTimeout {
					a.deleteContainerStatsSync(ctr.IdShort)
				}
				// retry once
				cstats, err = a.getContainerStats(ctr)
				if err != nil {
					slog.Error("Error getting container stats", "err", err)
					return
				}
			}
			containerStatsMutex.Lock()
			defer containerStatsMutex.Unlock()
			containerStats = append(containerStats, cstats)
		}()
	}

	wg.Wait()

	// remove old / invalid container stats
	for id := range a.containerStatsMap {
		if _, exists := validIds[id]; !exists {
			delete(a.containerStatsMap, id)
		}
	}

	return containerStats, nil
}

func (a *Agent) getContainerStats(ctr container.ApiInfo) (container.Stats, error) {
	cStats := container.Stats{}

	resp, err := a.dockerClient.Get("http://localhost/containers/" + ctr.IdShort + "/stats?stream=0&one-shot=1")
	if err != nil {
		return cStats, err
	}
	defer resp.Body.Close()

	// decode the json data from the response body
	var statsJson container.ApiStats
	if err := json.NewDecoder(resp.Body).Decode(&statsJson); err != nil {
		return cStats, err
	}

	name := ctr.Names[0][1:]

	// check if container has valid data, otherwise may be in restart loop (#103)
	if statsJson.MemoryStats.Usage == 0 {
		return cStats, fmt.Errorf("%s - no memory stats - see https://github.com/henrygd/beszel/issues/144", name)
	}

	// memory (https://docs.docker.com/reference/cli/docker/container/stats/)
	memCache := statsJson.MemoryStats.Stats["inactive_file"]
	if memCache == 0 {
		memCache = statsJson.MemoryStats.Stats["cache"]
	}
	usedMemory := statsJson.MemoryStats.Usage - memCache

	a.containerStatsMutex.Lock()
	defer a.containerStatsMutex.Unlock()

	// add empty values if they doesn't exist in map
	stats, initialized := a.containerStatsMap[ctr.IdShort]
	if !initialized {
		stats = &container.PrevContainerStats{}
		a.containerStatsMap[ctr.IdShort] = stats
	}

	// cpu
	cpuDelta := statsJson.CPUStats.CPUUsage.TotalUsage - stats.Cpu[0]
	systemDelta := statsJson.CPUStats.SystemUsage - stats.Cpu[1]
	cpuPct := float64(cpuDelta) / float64(systemDelta) * 100
	if cpuPct > 100 {
		return cStats, fmt.Errorf("%s cpu pct greater than 100: %+v", name, cpuPct)
	}
	stats.Cpu = [2]uint64{statsJson.CPUStats.CPUUsage.TotalUsage, statsJson.CPUStats.SystemUsage}

	// network
	var total_sent, total_recv uint64
	for _, v := range statsJson.Networks {
		total_sent += v.TxBytes
		total_recv += v.RxBytes
	}
	var sent_delta, recv_delta float64
	// prevent first run from sending all prev sent/recv bytes
	if initialized {
		secondsElapsed := time.Since(stats.Net.Time).Seconds()
		sent_delta = float64(total_sent-stats.Net.Sent) / secondsElapsed
		recv_delta = float64(total_recv-stats.Net.Recv) / secondsElapsed
	}
	stats.Net.Sent = total_sent
	stats.Net.Recv = total_recv
	stats.Net.Time = time.Now()

	cStats.Name = name
	cStats.Cpu = twoDecimals(cpuPct)
	cStats.Mem = bytesToMegabytes(float64(usedMemory))
	cStats.NetworkSent = bytesToMegabytes(sent_delta)
	cStats.NetworkRecv = bytesToMegabytes(recv_delta)

	return cStats, nil
}

func (a *Agent) gatherStats() system.CombinedData {
	systemInfo, systemStats := a.getSystemStats()
	systemData := system.CombinedData{
		Stats: systemStats,
		Info:  systemInfo,
	}
	// add docker stats
	if containerStats, err := a.getDockerStats(); err == nil {
		systemData.Containers = containerStats
	}
	// add extra filesystems
	systemData.Stats.ExtraFs = make(map[string]*system.FsStats)
	for name, stats := range a.fsStats {
		if !stats.Root && stats.DiskTotal > 0 {
			systemData.Stats.ExtraFs[name] = stats
		}
	}
	return systemData
}

func (a *Agent) Run() {
	// Create map for disk stats
	a.fsStats = make(map[string]*system.FsStats)

	// Set up slog with a log level determined by the LOG_LEVEL env var
	if logLevelStr, exists := os.LookupEnv("LOG_LEVEL"); exists {
		switch strings.ToLower(logLevelStr) {
		case "debug":
			slog.SetLogLoggerLevel(slog.LevelDebug)
		case "warn":
			slog.SetLogLoggerLevel(slog.LevelWarn)
		case "error":
			slog.SetLogLoggerLevel(slog.LevelError)
		}
	}

	// Set sensors context (allows overriding sys location for sensors)
	if sysSensors, exists := os.LookupEnv("SYS_SENSORS"); exists {
		slog.Info("SYS_SENSORS", "path", sysSensors)
		a.sensorsContext = context.WithValue(a.sensorsContext,
			common.EnvKey, common.EnvMap{common.HostSysEnvKey: sysSensors},
		)
	}

	a.initializeDiskInfo()
	a.initializeNetIoStats()

	a.startServer()
}
