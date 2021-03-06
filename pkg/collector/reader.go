/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/sustainable-computing-io/kepler/pkg/attacher"
	"github.com/sustainable-computing-io/kepler/pkg/model"
	"github.com/sustainable-computing-io/kepler/pkg/pod_lister"
	"github.com/sustainable-computing-io/kepler/pkg/power/acpi"
	"github.com/sustainable-computing-io/kepler/pkg/power/gpu"
	"github.com/sustainable-computing-io/kepler/pkg/power/rapl"
	"github.com/sustainable-computing-io/kepler/pkg/power/rapl/source"
)

// #define CPU_VECTOR_SIZE 128
import "C"

//TODO in sync with bpf program
type CgroupTime struct {
	CGroupPID      uint64
	PID            uint64
	ProcessRunTime uint64
	CPUCycles      uint64
	CPUInstr       uint64
	CacheMisses    uint64
	Command        [16]byte
	CPUTime        [C.CPU_VECTOR_SIZE]uint16
}

type ContainerEnergy struct {
	CGroupPID     uint64
	PID           uint64
	ContainerName string
	Namespace     string
	Command       string

	AggCPUTime     float64
	AggCPUCycles   uint64
	AggCPUInstr    uint64
	AggCacheMisses uint64

	CurrCPUTime     float64
	CurrCPUCycles   uint64
	CurrCPUInstr    uint64
	CurrCacheMisses uint64
	CurrResidentMem uint64

	CurrEnergyInCore  uint64
	CurrEnergyInDram  uint64
	CurrEnergyInOther uint64
	CurrEnergyInGPU   uint64
	AggEnergyInCore   uint64
	AggEnergyInDram   uint64
	AggEnergyInOther  uint64
	AggEnergyInGPU    uint64

	Disks          int
	CurrBytesRead  uint64
	CurrBytesWrite uint64
	AggBytesRead   uint64
	AggBytesWrite  uint64

	AvgCPUFreq float64
}

type CurrEdgeDeviceEnergy struct {
	CPUTime       float64
	CPUCycles     uint64
	CPUInstr      uint64
	CacheMisses   uint64
	EdgeDeviceMem float64

	EnergyInCore  float64
	EnergyInDram  float64
	EnergyInOther float64
	EnergyInGPU   float64
}

const (
	samplePeriod = 3000 * time.Millisecond
)

var (
	containerEnergy      = map[string]*ContainerEnergy{}
	EdgeDeviceEnergy     = map[string]float64{}
	gpuEnergy            = map[uint32]float64{}
	currEdgeDeviceEnergy = &CurrEdgeDeviceEnergy{}
	cpuFrequency         = map[int32]uint64{}
	EdgeDeviceName, _    = os.Hostname()
	cpuArch              = "unknown"
	acpiPowerMeter       = acpi.NewACPIPowerMeter()
	numCPUs              = runtime.NumCPU()
	lock                 sync.Mutex
)

func init() {
	arch, err := source.GetCPUArchitecture()
	if err == nil {
		cpuArch = arch
	}
}

func (c *Collector) reader() {
	ticker := time.NewTicker(samplePeriod)
	go func() {
		lastEnergyCore, _ := rapl.GetEnergyFromCore()
		lastEnergyDram, _ := rapl.GetEnergyFromDram()
		_ = gpu.GetGpuEnergy() // reset power usage counter

		acpiPowerMeter.Run()
		for {
			select {
			case <-ticker.C:
				cpuFrequency = acpiPowerMeter.GetCPUCoreFrequency()
				EdgeDeviceEnergy, _ = acpiPowerMeter.GetEnergyFromHost()

				var aggCPUTime, avgFreq, totalCPUTime float64
				var aggCPUCycles, aggCPUInstr, aggCacheMisses, aggBytesRead, aggBytesWrite uint64
				avgFreq = 0
				totalCPUTime = 0
				energyCore, err := rapl.GetEnergyFromCore()
				if err != nil {
					log.Printf("failed to get core power: %v\n", err)
					continue
				}
				energyDram, err := rapl.GetEnergyFromDram()
				if err != nil {
					log.Printf("failed to get dram power: %v\n", err)
					continue
				}
				if energyCore < lastEnergyCore || energyDram < lastEnergyDram {
					log.Printf("failed to get latest core or dram energy. Core energy %v should be more than %v; Dram energy %v should be more than %v\n",
						energyCore, lastEnergyCore, energyDram, lastEnergyDram)
				}
				coreDelta := float64(energyCore - lastEnergyCore)
				dramDelta := float64(energyDram - lastEnergyDram)
				if coreDelta == 0 && dramDelta == 0 {
					log.Printf("power reading not changed, retry\n")
					continue
				}
				gpuDelta := float64(0)
				for _, e := range gpuEnergy {
					gpuDelta += e
				}
				lastEnergyCore = energyCore
				lastEnergyDram = energyDram

				// calculate the total energy consumed in node from all sensors
				var nodeEnergyTotal float64 = 0
				for _, energy := range EdgeDeviceEnergy {
					nodeEnergyTotal += energy
				}
				// calculate the other energy consumed besides CPU/GPU and memory
				otherDelta := float64(0)
				if nodeEnergyTotal > 0 {
					otherDelta = nodeEnergyTotal - coreDelta - dramDelta - gpuDelta
				}

				lock.Lock()

				var ct CgroupTime
				aggCPUTime = 0
				aggCPUCycles = 0
				aggCacheMisses = 0
				aggCPUCycles = 0
				aggCPUInstr = 0
				aggBytesRead = 0
				aggBytesWrite = 0
				cgroupIO := make(map[uint64]bool)
				gpuEnergy, _ = gpu.GetCurrGpuEnergyPerPid()
				for _, v := range containerEnergy {
					v.CurrCPUCycles = 0
					v.CurrCPUTime = 0

					v.CurrCacheMisses = 0
					v.CurrCPUInstr = 0
					v.CurrBytesRead = 0
					v.CurrBytesWrite = 0
				}
				for it := c.modules.Table.Iter(); it.Next(); {
					data := it.Leaf()
					err := binary.Read(bytes.NewBuffer(data), binary.LittleEndian, &ct)
					if err != nil {
						log.Printf("failed to decode received data: %v", err)
						continue
					}
					comm := (*C.char)(unsafe.Pointer(&ct.Command))
					// fmt.Printf("pid %v cgroup %v cmd %v\n", ct.PID, ct.CGroupPID, C.GoString(comm))
					containerName, err := pod_lister.GetPodNameFromcGgroupID(ct.CGroupPID)
					if err != nil {
						log.Printf("failed to resolve pod for cGroup ID %v: %v", ct.CGroupPID, err)
						continue
					}
					if _, ok := containerEnergy[containerName]; !ok {
						containerEnergy[containerName] = &ContainerEnergy{}
						containerEnergy[containerName].ContainerName = containerName
						containerNamespace, err := pod_lister.GetPodNameSpaceFromcGgroupID(ct.CGroupPID)
						if err != nil {
							log.Printf("failed to find namespace for cGroup ID %v: %v", ct.CGroupPID, err)
							containerNamespace = "unknown"
						}
						containerEnergy[containerName].Namespace = containerNamespace
						containerEnergy[containerName].CGroupPID = ct.CGroupPID
						containerEnergy[containerName].PID = ct.PID
						containerEnergy[containerName].Command = C.GoString(comm)
					}
					if attacher.EnableCPUFreq {
						avgFreq, totalCPUTime = getAVGCPUFreqAndTotalCPUTime(cpuFrequency, ct.CPUTime)
					} else {
						totalCPUTime = float64(ct.ProcessRunTime)
					}
					// to prevent overflow of the counts we change the unit to have smaller numbers
					totalCPUTime = totalCPUTime / 1000
					containerEnergy[containerName].CurrCPUTime += totalCPUTime
					containerEnergy[containerName].AggCPUTime += totalCPUTime
					aggCPUTime += totalCPUTime
					val := ct.CPUCycles
					containerEnergy[containerName].CurrCPUCycles += val
					containerEnergy[containerName].AggCPUCycles += val
					aggCPUCycles += val
					val = ct.CPUInstr
					containerEnergy[containerName].CurrCPUInstr += val
					containerEnergy[containerName].AggCPUInstr += val
					aggCPUInstr += val
					val = ct.CacheMisses
					containerEnergy[containerName].CurrCacheMisses += val
					containerEnergy[containerName].AggCacheMisses += val
					aggCacheMisses += val

					containerEnergy[containerName].AvgCPUFreq = avgFreq
					if e, ok := gpuEnergy[uint32(ct.PID)]; ok {
						// fmt.Printf("gpu energy pod %v comm %v pid %v: %v\n", containerName, C.GoString(comm), ct.PID, e)
						containerEnergy[containerName].CurrEnergyInGPU += uint64(e)
						containerEnergy[containerName].AggEnergyInGPU += containerEnergy[containerName].CurrEnergyInGPU
					}
					rBytes, wBytes, disks, err := pod_lister.ReadCgroupIOStat(ct.CGroupPID)
					// fmt.Printf("read %d write %d. Agg read %d write %d, err %v\n", rBytes, wBytes, aggBytesRead, aggBytesWrite, err)
					if err == nil {
						// if this is the first time the cgroup's I/O is accounted, add it to the pod
						if _, ok := cgroupIO[ct.CGroupPID]; !ok {
							cgroupIO[ct.CGroupPID] = true
							if disks > containerEnergy[containerName].Disks {
								containerEnergy[containerName].Disks = disks
							}
							// save the current I/O in CurrByteRead and adjust it later
							containerEnergy[containerName].CurrBytesRead += rBytes
							aggBytesRead += rBytes
							containerEnergy[containerName].CurrBytesWrite += wBytes
							aggBytesWrite += wBytes
						}
					}
				}
				// reset all counters in the eBPF table
				c.modules.Table.DeleteAll()
				totalReadBytes, totalWriteBytes, disks, err := pod_lister.ReadAllCgroupIOStat()
				if err == nil {
					if totalReadBytes > aggBytesRead && totalWriteBytes > aggBytesWrite {
						rBytes := totalReadBytes - aggBytesRead
						wBytes := totalWriteBytes - aggBytesWrite
						podName := pod_lister.GetSystemProcessName()
						containerEnergy[podName].Disks = disks
						containerEnergy[podName].CurrBytesRead = rBytes
						containerEnergy[podName].CurrBytesWrite = wBytes
					} else {
						fmt.Printf("total read %d write %d should be greater than agg read %d agg write %d\n", totalReadBytes, totalWriteBytes, aggBytesRead, aggBytesWrite)
					}
				}

				//evenly attribute other energy among all pods
				perProcessOtherMJ := float64(otherDelta / float64(len(containerEnergy)))

				_, podMem, _, EdgeDeviceMem, err := pod_lister.GetPodMetrics()
				if err != nil {
					fmt.Printf("failed to get kubelet metrics: %v", err)
				}

				log.Printf("energy count: core %.2f dram: %.2f time %.6f cycles %d instructions %d misses %d EdgeDevice memory %f\n",
					coreDelta, dramDelta, aggCPUTime, aggCPUCycles, aggCPUInstr, aggCacheMisses, EdgeDeviceMem)
				currEdgeDeviceEnergy = &CurrEdgeDeviceEnergy{
					CPUTime:       aggCPUTime,
					CPUCycles:     aggCPUCycles,
					CPUInstr:      aggCPUInstr,
					CacheMisses:   aggCacheMisses,
					EdgeDeviceMem: EdgeDeviceMem,
					EnergyInCore:  coreDelta,
					EnergyInDram:  dramDelta,
					EnergyInOther: otherDelta,
					EnergyInGPU:   gpuDelta,
				}
				for containerName, v := range containerEnergy {
					cpuTimeRatio := float64(0.0)
					cpuCycleRatio := float64(0.0)
					cpuInstrRatio := float64(0.0)
					dyMemRatio := float64(0.0)
					bgMemRatio := float64(0.0)

					if v.CurrCPUTime > 0 {
						cpuTimeRatio = float64(float64(v.CurrCPUTime)/aggCPUTime) * coreDelta * model.RunTimeCoeff.CPUTime
					}
					if v.CurrCPUCycles > 0 {
						cpuCycleRatio = float64(v.CurrCPUCycles) / float64(aggCPUCycles) * coreDelta * model.RunTimeCoeff.CPUCycle
					}
					if v.CurrCPUInstr > 0 {
						cpuInstrRatio = float64(v.CurrCPUInstr) / float64(aggCPUInstr) * coreDelta * model.RunTimeCoeff.CPUInstr
					}

					v.CurrEnergyInCore = uint64(cpuTimeRatio + cpuCycleRatio + cpuInstrRatio)
					v.AggEnergyInCore += v.CurrEnergyInCore

					if v.CurrCacheMisses > 0 {
						dyMemRatio = float64(v.CurrCacheMisses) / float64(aggCacheMisses) * dramDelta * model.RunTimeCoeff.CacheMisses
					}
					k := v.Namespace + "/" + containerName
					if mem, ok := podMem[k]; ok {
						v.CurrResidentMem = uint64(mem)
						bgMemRatio = float64(mem/EdgeDeviceMem) * dramDelta * model.RunTimeCoeff.MemoryUsage
					}
					v.CurrEnergyInDram = uint64(dyMemRatio + bgMemRatio)
					v.AggEnergyInDram += v.CurrEnergyInDram
					v.CurrEnergyInOther = uint64(perProcessOtherMJ)
					v.AggEnergyInOther += uint64(perProcessOtherMJ)

					val := uint64(0)
					if v.CurrBytesRead >= v.AggBytesRead {
						val = v.CurrBytesRead - v.AggBytesRead
						v.AggBytesRead = v.CurrBytesRead
						v.CurrBytesRead = val
					}
					if v.CurrBytesWrite >= v.AggBytesWrite {
						val = v.CurrBytesWrite - v.AggBytesWrite
						v.AggBytesWrite = v.CurrBytesWrite
						v.CurrBytesWrite = val
					}

					if v.CurrEnergyInCore > 0 {
						log.Printf("\tenergy from pod: name: %s namespace: %s \n"+
							"\teCore: %d(%d) eDram: %d(%d) eOther: %d(%d) eGPU: %d(%d) \n"+
							"\tCPUTime: %.2f (%.4f) \n\tcycles: %d (%.4f) \n\tinstructions: %d (%.4f) \n"+
							"\tDiskReadBytes: %d (%d) \n\tDiskWriteBytes: %d (%d)\n"+
							"\tmisses: %d (%.4f)\tResidentMemRatio: %.4f\n\tavgCPUFreq: %.4f MHZ\n\tpid: %v comm: %v\n",
							containerName, v.Namespace,
							v.CurrEnergyInCore, v.AggEnergyInCore,
							v.CurrEnergyInDram, v.AggEnergyInDram,
							v.CurrEnergyInOther, v.AggEnergyInOther,
							v.CurrEnergyInGPU, v.AggEnergyInGPU,
							v.CurrCPUTime, float64(v.CurrCPUTime)/float64(aggCPUTime),
							v.CurrCPUCycles, float64(v.CurrCPUCycles)/float64(aggCPUCycles),
							v.CurrCPUInstr, float64(v.CurrCPUInstr)/float64(aggCPUInstr),
							v.CurrBytesRead, v.AggBytesRead,
							v.CurrBytesRead, v.AggBytesWrite,
							v.CurrCacheMisses, float64(v.CurrCacheMisses)/float64(aggCacheMisses),
							float64(v.CurrResidentMem)/EdgeDeviceMem,
							v.AvgCPUFreq/1000, /*MHZ*/
							v.PID, v.Command)
					}
				}
				lock.Unlock()
			}
		}
	}()
}

// getAVGCPUFreqAndTotalCPUTime calculates the weighted cpu frequency average
func getAVGCPUFreqAndTotalCPUTime(cpuFrequency map[int32]uint64, cpuTime [C.CPU_VECTOR_SIZE]uint16) (float64, float64) {
	totalFreq := float64(0)
	totalCPUTime := float64(0)
	for cpu, freq := range cpuFrequency {
		if cpuTime[cpu] != 0 {
			totalFreq += float64(freq) * float64(cpuTime[cpu])
			totalCPUTime += float64(cpuTime[cpu])
		}
	}
	avgFreq := totalFreq / totalCPUTime
	return avgFreq, totalCPUTime
}
