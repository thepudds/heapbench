# heapbench
A simple garbage collector benchmarking utility for Go that allows specifying a garbage generation rate, a rate of leaking, a baseline amount of live memory, and a baseline level of pure CPU work via fake jobs.

The garbage rate is translated into a corresponding amount of garbage generated per job,
while the leaking occurs at a constant rate in the background.

heapbench reports its own RSS and process CPU usage to stderr. You will usually want to also
`export GODEBUG=gctrace=1` or similar.

Primarily tested on Linux. macOS and Windows likely work.

### Install

```
go install github.com/thepudds/heapbench@latest
```

### Examples

 1. **Pure CPU work without GC work**

    This example uses ~2 cores:

    ```
    heapbench -baseheap=0 -garbagerate=0 -leakrate=0 -jobrate=100 -worktime=20ms
    ```

    This specifies an average job arrival rate of 100/sec (i.e., 10ms average between job starts),
    with each job averaging 20ms of CPU wok, and no material memory being allocated.
    Jobs arriving twice as fast as the quantity of work in each job means it 
    uses very close to 2 CPU cores on average (e.g., as seen via `top -d 60` or similar).

 2. **CPU + GC work**
    
    This adds in a base heap, garbage generation, and leak:

    ```
    heapbench -baseheap=128 -garbagerate=128 -leakrate=1 -jobrate=100 -worktime=20ms
    ```

    Here we have the same pure CPU work as the prior example, but now with a baseline of 128 MB of live memory (memory being held onto), which increases at a
    rate of 1MB/sec (simulating a leak), and an average of 128 MB/sec of garbage
    generated (memory created but not held onto). Live memory will start at 128 MB, then
    creep up by 1 MB every second until the process dies or is stopped.

### Flags

```
Usage of heapbench

Memory:
  -baseheap MiB
        initial amount of memory in MiB to allocate and hold onto forever. (default 128)
  -garbagerate MiB/sec
        rate of memory in MiB/sec to allocate without holding onto it. (default 16)
  -leakrate MiB/sec
        rate of memory in MiB/sec to allocate and hold onto forever. (default 1)

CPU:
  -jobrate jobs/sec
        average arrival rate in jobs/sec. For example, with '-arrivalrate=100 -worktime=20ms', 
        2 jobs will be getting processed simultaneously on average. (default 100)
  -worktime duration
        average service time for each job. Cannot be set with -workloops.
  -workloops million
        do an average of N million tight loops per job. A value of 1 translates to roughly
        5-20ms, depending on hardware. Cannot be set with -worktime.

Logging:         
  -stats duration
        frequency of logging RSS and CPU usage. CPU reflects the last measurement period, 
        with 100% representing 1 logical core. RSS is instantaneous measure. (default 60s)
```

### Additional Details

The job inter-arrival times and CPU work per job are exponentially distributed (roughly
an M/M/N queue with processor sharing), which gives some short timescale variability
that yields consistent average rates of CPU usage and garbage generation on
longer timescales (e.g., reasonably consistent averages measured over multiple minutes when
input job parameters are on order of 10s of milliseconds).

heapbench is primarily meant to be run with job durations that are on the order of 1ms to 100ms.
Outside of that range, shorter jobs have more per-job overhead (which can be OK),
while longer jobs have more variance over a given measurement interval (which can require
more patience for meaningful averages).
