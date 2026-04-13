package config

import "github.com/EXBO-Community/stalcraft-jvm-optimization/internal/sysinfo"

// Generate produces a performance-oriented Config for the given hardware.
//
// The profile targets a single goal: STALCRAFT running as smoothly as
// possible on a default.json. Values are NOT scaled down to save
// resources — we pick the largest safe number every time.
//
// Only heap size, G1 region size and GC thread count actually depend
// on memory and core count; the JIT/inlining block is scaled by L3
// cache size (X3D-class parts get deeper inlining and a larger node
// budget because their compiled hot path fits entirely in L3).
// Everything else is a fixed, tested default compatible with OpenJDK 9.
func Generate(sys sysinfo.Info) Config {
	heap := sizeHeap(sys.TotalGB())
	parallel, concurrent := gcThreads(sys.CPUCores)
	jit := jitProfile(sys)
	ihop := 20
	// 40 ms is a realistic target on consumer CPUs with DDR4 / DDR5
	// without V-Cache — setting it much lower just forces G1 to slice
	// young GC into smaller, more frequent pauses and hurts throughput
	// without actually reaching the target.
	pauseMs := 40
	if sys.HasBigCache() {
		// On X3D-class CPUs we can start concurrent marking earlier —
		// memory bandwidth headroom is effectively free, and earlier
		// marking reduces the risk of mixed-GC pressure. The huge L3
		// also lets G1 realistically hit a tighter pause target.
		ihop = 15
		pauseMs = 25
		if sys.CPUCores >= 8 {
			concurrent++
		}
	}

	return Config{
		HeapSizeGB:  int(heap),
		PreTouch:    sys.TotalGB() >= 12,
		MetaspaceMB: 512,

		MaxGCPauseMillis:               pauseMs,
		G1HeapRegionSizeMB:             regionSize(heap),
		G1NewSizePercent:               30,
		G1MaxNewSizePercent:            50,
		G1ReservePercent:               20,
		G1HeapWastePercent:             5,
		G1MixedGCCountTarget:           4,
		InitiatingHeapOccupancyPercent: ihop,
		G1MixedGCLiveThresholdPercent:  90,
		G1RSetUpdatingPauseTimePercent: 0,
		SurvivorRatio:                  32,
		MaxTenuringThreshold:           1,

		G1SATBBufferEnqueueingThresholdPercent: 30,
		G1ConcRSHotCardLimit:                   16,
		G1ConcRefinementServiceIntervalMillis:  150,
		GCTimeRatio:                            99,
		UseDynamicNumberOfGCThreads:            true,
		UseStringDeduplication:                 true,

		ParallelGCThreads:       parallel,
		ConcGCThreads:           concurrent,
		SoftRefLRUPolicyMSPerMB: 50,

		ReservedCodeCacheSizeMB: 400,
		MaxInlineLevel:          jit.maxInlineLevel,
		FreqInlineSize:          jit.freqInlineSize,
		InlineSmallCode:         jit.inlineSmallCode,
		MaxNodeLimit:            jit.maxNodeLimit,
		NodeLimitFudgeFactor:    8000,
		NmethodSweepActivity:    1,
		DontCompileHugeMethods:  false,
		AllocatePrefetchStyle:   3,
		AlwaysActAsServerClass:  true,
		UseXMMForArrayCopy:      true,
		UseFPUForSpilling:       true,

		UseLargePages: sys.LargePages,

		ReflectionInflationThreshold: 0,
		AutoBoxCacheMax:              4096,
		UseThreadPriorities:          true,
		ThreadPriorityPolicy:         1,
		UseCounterDecay:              false,
		CompileThresholdScaling:      0.5,
		EnableJVMLog:                 true,
	}
}

// jitProfile scales C2 inlining limits with L3 cache. On normal CPUs
// a deeply inlined hot path spills out of L3; on X3D-class parts the
// full compiled working set fits, so deeper inlining is pure win.
type jitLimits struct {
	maxInlineLevel  int
	freqInlineSize  int
	inlineSmallCode int
	maxNodeLimit    int
}

func jitProfile(sys sysinfo.Info) jitLimits {
	if sys.HasBigCache() {
		return jitLimits{
			maxInlineLevel:  20,
			freqInlineSize:  750,
			inlineSmallCode: 6000,
			maxNodeLimit:    320000,
		}
	}
	return jitLimits{
		maxInlineLevel:  15,
		freqInlineSize:  500,
		inlineSmallCode: 4000,
		maxNodeLimit:    240000,
	}
}

// sizeHeap picks a heap size between 2 and 8 GB based on total RAM.
//
// We cap at 8 GB on purpose: STALCRAFT's live working set is ~2-3 GB,
// and larger heaps only inflate G1 scan time without helping throughput.
// The 2 GB floor is the minimum that lets G1 run efficiently; anything
// below and the game runs, but full GCs dominate.
func sizeHeap(totalGB uint64) uint64 {
	switch {
	case totalGB >= 24:
		return 8
	case totalGB >= 16:
		return 6
	case totalGB >= 12:
		return 5
	case totalGB >= 8:
		return 4
	case totalGB >= 6:
		return 3
	default:
		return 2
	}
}

// gcThreads derives the STW and concurrent GC worker counts from the
// physical core count, assuming 2-way SMT (every modern consumer CPU).
//
// Parallel workers only run during STW — the game thread is stopped
// anyway, so HT siblings are free to do GC work without contention.
// That's why we scale off total logical threads (cores × 2), not just
// physical cores, and cap at 10 where G1 hits diminishing returns.
//
// Concurrent workers share CPU with the running game, so they stay
// a bit more conservative: roughly half of parallel, floor 1, ceiling 4.
func gcThreads(cores int) (parallel, concurrent int) {
	parallel = clamp(cores*2-2, 2, 10)
	concurrent = clamp(parallel/2, 1, 4)
	return
}

// regionSize matches G1 region granularity to heap size. JVM only
// accepts powers of two between 1 and 32 MB; larger regions mean fewer
// RSet scans, smaller regions mean finer mixed-GC control. sizeHeap
// caps heap at 8 GB, so 16 MB is the upper choice in practice —
// 32 MB regions would leave only 256 regions at 8 GB heap, hurting
// mixed-GC selection granularity.
func regionSize(heapGB uint64) int {
	switch {
	case heapGB <= 3:
		return 4
	case heapGB <= 5:
		return 8
	default:
		return 16
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
