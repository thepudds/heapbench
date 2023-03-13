// heapbench is a simple GC benchmarking utility that allows specifying a garbage generation rate,
// a rate of leaking, a baseline amount of live memory, and a baseline level of pure CPU work
// via fake jobs.
//
// The garbage rate is translated into a corresponding amount of garbage generated per job,
// while the leaking occurs at a constant rate in the background.
//
// Two example invocations:
//
//  1. Pure CPU work without GC work -- here, an average job arrival rate of 100/sec (i.e., 10ms average between job starts),
//     with each job averaging 20ms of CPU wok, and no material memory being allocated.
//     Jobs arriving twice as fast as the quantity of work in each job means this example
//     uses very close to 2 CPU cores on average (e.g., as seen via 'top -d 60' or similar):
//
//     heapbench -baseheap=0 -garbagerate=0 -leakrate=0 -jobrate=100 -worktime=20ms
//
//  2. CPU + GC work -- same pure CPU work as prior example, but adding in GC work as well,
//     with a baseline of 128 MB of live memory (memory being held onto), which increases at a
//     rate of 1MB/sec (simulating a leak), and an average of 128 MB/sec of garbage
//     generated (memory created but not held onto). Live memory will start at 128 MB, then
//     creep up by 1 MB every second until the process dies or is stopped.
//
//     heapbench -baseheap=128 -garbagerate=128 -leakrate=1 -jobrate=100 -worktime=20ms
//
// The job inter-arrival times and CPU work per job are exponentially distributed (roughly
// an M/M/N queue with processor sharing), which gives some short timescale variability
// that yields consistent average rates of CPU usage and garbage generation on
// longer timescales (e.g., reasonably consistent averages measured over multiple minutes when
// input job parameters are on order of 10s of milliseconds).
//
// heapbench is primarily meant to be run with job durations that are on the order of 1ms to 100ms.
// Outside of that range, shorter jobs have more per-job overhead (which can be OK),
// while longer jobs have more variance over a given measurement interval (which can require
// more patience for meaningful averages).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"time"
	"unsafe" // for unsafe.Sizeof

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	pointersPerNode = 4 // includes next pointer, so must be at least 1
	intsPerNode     = 4
)

// node is used to build up live memory, arranged in a series of linked lists of nodes.
type node struct {
	pointers [pointersPerNode - 1]*node
	next     *node
	_        [intsPerNode]int
}

// job is a unit of work. We only set duration or loopCount.
type job struct {
	duration  time.Duration // spin duration for one job
	loopCount int           // number of decoding loops for one job
	garbage   memBytes      // bytes of garbage to create in one job
}

// Some units.
type (
	memBytes       int
	memBytesPerSec int
)

func main() {
	start := time.Now()
	logf := func(format string, a ...any) {
		now := time.Now()
		fmt.Fprintf(os.Stderr, fmt.Sprintf("heapbench: %s @%.3fs ", now.Format("15:04:05.000"), now.Sub(start).Seconds())+format+"\n",
			a...)
	}

	jobArrivalRate := flag.Float64("jobrate", 100, "average arrival rate in `jobs/sec`. For example, with '-arrivalrate=100 -worktime=20ms', 2 jobs will be getting processed simultaneously on average.")
	workTime := flag.Duration("worktime", 0, "average service time for each job. Cannot be set with -workloops")
	workLoops := flag.Float64("workloops", 0, "do an average of N `million` tight loops per job. A value of 1 translates to roughly 5-20ms, depending on hardware. Cannot be set with -worktime")
	baseHeap := flag.Float64("baseheap", 128, "initial amount of memory in `MiB` to allocate and hold onto forever.")
	leakRate := flag.Float64("leakrate", 1, "rate of memory in `MiB/sec` to allocate and hold onto forever.")
	garbageRate := flag.Float64("garbagerate", 16, "rate of memory in `MiB/sec` to allocate without holding onto it.")
	stats := flag.Duration("stats", 60*time.Second, "frequency of logging RSS and CPU usage. CPU reflects the last measurment period, with 100% representing 1 logical core. RSS is instantaneous measure.")
	flag.Parse()

	// Do basic sanity check of input params.
	if *workTime != 0 && *workLoops != 0 {
		logf("cannot simultaneously set -worktime and -workloops")
		os.Exit(2)
	}
	if *jobArrivalRate < 1 {
		logf("low job arrival rates lead to lumpy garbage generation and work, and might not be what you expect")
		os.Exit(2)
	}

	// If requested, prepare for fake CPU work of decoding varints repeatedly in each job.
	// (Otherwise, we just call time.Now repeatedly to control how long we are in a spin loop for each job).
	workAvg := *workTime
	caveat := ""
	if *workLoops != 0 {
		// Create random values, encode them as varints, and measure how long it takes to repeatedly decode them.
		logf("measuring performance of fake CPU work (decoding varints)...")
		workAvg = prepareFakeCPUWork(int(*workLoops * 1e6))
		logf("avg job fake CPU work loops: %.1f M", *workLoops)
		caveat = "(estimate based on measurement)" // workAvg and jobsAvg will be estimates
	}

	// Calculate some derived parameters.
	interArrivalAvg := time.Duration((1 / *jobArrivalRate) * float64(time.Second))
	jobsAvg := float64(workAvg) / float64(interArrivalAvg)
	bytesPerJob := memBytes(*garbageRate * 1024 * 1024 * interArrivalAvg.Seconds())

	// Print the rest of our parameters.
	logf("avg job inter-arrival time: %s", interArrivalAvg)
	logf("avg job service time: %v %s", workAvg, caveat)
	logf("avg simultaneous jobs: %.2f %s", jobsAvg, caveat)
	logf("base heap: %.1f MiB", *baseHeap)
	logf("leak rate: %.1f MiB/s", *leakRate)
	logf("garbage rate: %.1f MiB/s", *garbageRate)
	logf("garbage per job: %d bytes", bytesPerJob)

	// Prepare base heap.
	logf("start allocation of base heap...")
	var liveMem liveMemory
	liveMem.add(memBytes(*baseHeap * 1024 * 1024))
	logf("finished allocation of base heap")

	// Log resource usage periodically.
	c := time.Tick(*stats)
	_, _ = resourceUsage() // start measuring CPU usage from here
	go func() {
		for range c {
			rss, cpuPct := resourceUsage()
			logf("rss: %.1f MiB, cpu: %.1f%%", float64(rss)/(1<<20), cpuPct)
		}
	}()

	// TODO: Probably remove. This uses 1 logical core. Can be used to sanity check our CPU usage logging.
	// logf("start spinning...")
	// s := time.Now()
	// for time.Since(s) < 15*time.Second {
	// }

	logf("start benchmark...")
	go liveMem.leak(memBytesPerSec(*leakRate * 1024 * 1024))
	generateJobs(interArrivalAvg, *workTime, *workLoops*1e6, bytesPerJob)
}

// generateJobs loops forever creating jobs.
func generateJobs(interArrivalAvg time.Duration, workTimeAvg time.Duration, workLoopsAvg float64, bytesPerJob memBytes) {
	var extraSleep time.Duration // Positive when we sleep longer than asked.
	for {
		// Calculate an inter-arrival time based on an exponential distribution, then sleep.
		// We track and correct if we sleep too long or not long enough.
		interArrival := expDuration(interArrivalAvg)
		desiredSleep := interArrival - extraSleep
		startSleep := time.Now()
		time.Sleep(desiredSleep)
		slept := time.Since(startSleep)
		extraSleep = slept - desiredSleep

		// Define the next job.
		j := job{garbage: bytesPerJob}
		if workLoopsAvg != 0 {
			// Calcuate the work for this job using an exponential distribution of loop counts.
			j.loopCount = int(expFloat(workLoopsAvg))
		} else {
			// Calculate how long we should work using an exponential distribution of service times.
			j.duration = expDuration(workTimeAvg)
		}

		// Kick off the job. We purposefully do not wait for it to complete.
		go processJob(j)
	}
}

// processJob does the work of one job, including creating garbage and doing
// fake CPU work as requested.
func processJob(j job) []byte {
	var throwaway []byte

	// Create garbage of variable size.
	// Currently averages roughly 512 bytes per alloc.
	// It is "roughly" because the exact average depends on how many we allocate,
	// there is rounding up to size classes, etc.
	count := 0
	for j.garbage > 0 {
		allocSize := memBytes(count%1024 + 1)
		if allocSize > j.garbage {
			allocSize = j.garbage
		}
		throwaway = make([]byte, allocSize)
		if len(throwaway) > 0 {
			throwaway[0] = 'x' // don't let it stay all zeros
		}
		j.garbage -= allocSize
		count++
	}

	// Spin based on duration if requested.
	// We purposefully measure the CPU work time outside of the allocations above,
	// including so that the spin duration is independent of any extra costs
	// that might happen during allocation, including when GC is falling behind.
	if j.duration != 0 {
		start := time.Now()
		for time.Since(start) < j.duration {
		}
		return throwaway
	}

	// Spin based on loop count if requested.
	fakeCPUWork(j.loopCount)
	return throwaway
}

// liveMemory is a slice of node linked lists.
// It is initially populated with approximately baseheap MB, and then grows at approximately leakrate MiB/sec.
// The "approximately" is because we don't track the size of the slice itself.
type liveMemory struct {
	nodes []*node
}

// add creates size bytes of memory, which we place in liveMemory.
// The memory is created as multiple linked lists of node objects, with
// each list having a max length of 10.
func (m *liveMemory) add(size memBytes) {
	nodeListDepth := 10
	nodeSize := memBytes(unsafe.Sizeof(node{}))
	count := int(size / nodeSize)
	created := 0
	for created < count {
		head := &node{}
		curr := head
		created++
		for i := 0; i < nodeListDepth-1 && created < count; i++ {
			for j := range curr.pointers {
				// Set the dummy pointers to something (head) so that we don't leave them nil.
				curr.pointers[j] = head
			}
			curr.next = &node{}
			curr = curr.next
			created++
		}
		m.nodes = append(m.nodes, head)
	}
}

// leak adds memory to our live memory in a loop at a constant rate.
func (m *liveMemory) leak(byteRate memBytesPerSec) {
	const leaksPerSec = 10
	bytesPerTicker := memBytes(byteRate / leaksPerSec)
	ticker := time.NewTicker(time.Second / leaksPerSec)
	for range ticker.C {
		m.add(bytesPerTicker)
	}
}

// varints holds input data for our fake CPU work.
var (
	varintCount = 10_000
	varints     = make([]byte, varintCount*binary.MaxVarintLen64)
)

// prepareFakeCPUWork randomly populates the varints global with varint encoded values,
// and then measures and reports average decode time for loopCount decodes.
func prepareFakeCPUWork(loopCount int) (avg time.Duration) {
	r := rand.New(rand.NewSource(0))
	offset := 0
	for i := 0; i < varintCount; i++ {
		// These are encoded with 1, 2, 3, or 4 bytes, respectively.
		val := []uint64{1 << 6, 1 << 13, 1 << 20, 1 << 27}[r.Intn(4)]
		offset += binary.PutUvarint(varints[offset:], val)
	}

	// Measure the work of a job 100 times to get a better average duration.
	start := time.Now()
	for i := 0; i < 100; i++ {
		fakeCPUWork(loopCount)
	}
	return time.Since(start) / time.Duration(100)
}

// fakeCPUWork does the fake work of repeatedly decoding our varints global.
func fakeCPUWork(loopCount int) uint64 {
	sum := uint64(0)
	offset := 0
	for i := 0; i < loopCount; i++ {
		if i%varintCount == 0 {
			offset = 0
		}
		val, n := binary.Uvarint(varints[offset:])
		sum += val
		offset += n
	}
	return sum
}

// resourceUsage reports our RSS in bytes and CPU percent utilization, where 100% is the equivalent of 1 logical core.
func resourceUsage() (rss int, cpuPct float64) {
	proc := must(process.NewProcess(int32(os.Getpid())))
	mem := must(proc.MemoryInfo())
	// Duration of 0 gives cpu usage since the last call.
	pcts := must(cpu.Percent(0, false))
	if len(pcts) != 1 {
		panic(fmt.Errorf("heapbench: expected a single total cpu percentage, got: %d", len(pcts)))
	}
	pct := pcts[0] * float64(runtime.NumCPU())
	return int(mem.RSS), pct
}

func expDuration(mean time.Duration) time.Duration {
	res := time.Duration(rand.ExpFloat64() * float64(mean))
	if res > 10*mean {
		// Clip in rare case of large value (clipping roughly at 99.99th percentile).
		res = 10 * mean
	}
	return res
}

func expFloat(mean float64) float64 {
	res := rand.ExpFloat64() * mean
	if res > 10*mean {
		// Clip in rare case of large value (clipping roughly at 99.99th percentile).
		res = 10 * mean
	}
	return res
}

func must[T any](t T, err error) T {
	if err != nil {
		panic(fmt.Errorf("heapbench: unexpected error: %v", err))
	}
	return t
}
