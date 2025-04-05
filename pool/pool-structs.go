package pool

import (
	"sync"
	"time"
)

// Only pointers can be stored in the pool, anything else will cause an error.
// (no panic will be thrown)
type pool struct {
	allocator func() any
	cleaner   func(any)
	pool      []any

	// Pass nil if you would like default config.
	config          *poolConfig
	Stats           *poolStats
	mu              *sync.RWMutex
	cond            *sync.Cond
	isShrinkBlocked bool
}

type shrinkDefaults struct {
	interval      time.Duration
	idle          time.Duration
	minIdle       int
	cooldown      time.Duration
	utilization   float64
	underutilized int
	percent       float64
	maxShrinks    int
}

type poolStats struct { // x
	objectsInUse          uint64  // x
	utilizationPercentage float64 // x
	availableObjects      uint64  // x
	peakInUse             uint64  // x

	totalGets  uint64  // x
	totalPuts  uint64  // x
	hitCount   uint64  // x
	missCount  uint64  // x
	hitRate    float64 // x
	missRate   float64 // x
	reuseRatio float64 // x

	totalGrowthEvents  uint64 // x
	totalShrinkEvents  uint64 // x
	consecutiveShrinks uint64 // x

	currentCapacity int // x
	initialCapacity int // x

	lastTimeCalledGet time.Time // x
	lastTimeCalledPut time.Time // x
	lastShrinkTime    time.Time // x
	lastGrowTime      time.Time // x
}

type poolConfig struct {
	// Pool initial capacity which avoids resizing the slice,
	// until it reaches the defined capacity.
	initialCapacity int

	// Determines how the pool grows.
	poolGrowthParameters *poolGrowthParameters

	// Determines how the pool shrinks.
	poolShrinkParameters *poolShrinkParameters
}

type poolShrinkParameters struct { // x
	// EnforceCustomConfig controls whether the pool requires explicit configuration.
	// When set to true, the user must manually provide all configuration values (e.g., shrink/growth parameters).
	// If set to false (default), the pool will fall back to built-in default configurations when values are missing.
	// This flag does not disable auto-shrink behavior—it only governs configuration strictness.
	enforceCustomConfig bool

	// AggressivenessLevel is an optional high-level control that adjusts
	// shrink sensitivity and timing behavior. Valid values range from 0 (disabled)
	// to higher levels (1–5), where higher levels cause faster and more frequent shrinking.
	// This can override individual parameter values.
	aggressivenessLevel aggressivenessLevel

	// CheckInterval controls how frequently the background shrink goroutine runs.
	// This determines how often the pool is evaluated for possible shrink conditions.
	checkInterval time.Duration // x

	// IdleThreshold is the minimum duration the pool must remain idle
	// (no calls to Get) before it can be considered for shrinking.
	idleThreshold time.Duration // x

	// MinIdleBeforeShrink defines how many consecutive idle checks
	// (based on IdleThreshold and CheckInterval) must occur before a shrink is allowed.
	// This prevents shrinking during short idle spikes.
	minIdleBeforeShrink int // x

	// ShrinkCooldown is the minimum amount of time that must pass between
	// two consecutive shrink operations. This prevents excessive or aggressive shrinking.
	shrinkCooldown time.Duration // x

	// MinUtilizationBeforeShrink defines the threshold for utilization ratio
	// (ObjectsInUse / CurrentCapacity) under which the pool is considered underutilized.
	// If the utilization stays below this value for StableUnderutilizationRounds,
	// the pool becomes a shrink candidate.
	minUtilizationBeforeShrink float64 // x

	// StableUnderutilizationRounds defines how many consecutive background checks
	// must detect underutilization before a shrink is triggered.
	// This avoids false positives caused by temporary usage dips.
	stableUnderutilizationRounds int // x

	// ShrinkStepPercent determines how much of the pool should be reduced
	// when a shrink operation is triggered (e.g. 0.25 = shrink by 25%).
	shrinkPercent float64 // x

	// MaxConsecutiveShrinks defines how many shrink operations can happen back-to-back
	// before the shrink logic pauses until a get request happens.
	// The default is 2, setting for less than two won't be allowed.
	maxConsecutiveShrinks int

	// MinCapacity defines the lowest allowed capacity after shrinking.
	// The pool will never shrink below this value, even under aggressive conditions.
	minCapacity int // x
}

type poolGrowthParameters struct {
	// Threshold multiplier that determines when to switch from exponential to fixed growth.
	// Once the capacity reaches (InitialCapacity * ExponentialThresholdFactor), the growth
	// strategy switches to fixed mode.
	//
	// Example:
	//   InitialCapacity = 12
	//   ExponentialThresholdFactor = 4.0
	//   Threshold = 12 * 4.0 = 48
	//
	//   → Pool grows exponentially until it reaches capacity 48,
	//     then it grows at a fixed pace.
	exponentialThresholdFactor float64

	// Growth percentage used while in exponential mode.
	// Determines how much the capacity increases as a percentage of the current capacity.
	//
	// Example:
	//   CurrentCapacity = 20
	//   GrowthPercent = 0.5 (50%)
	//   Growth = 20 * 0.5 = 10 → NewCapacity = 30
	//
	//   → Pool grows: 12 → 18 → 27 → 40 → 60 → ...
	growthPercent float64

	// Once in fixed growth mode, this fixed value is added to the current capacity
	// each time the pool grows.
	//
	// Example:
	//   InitialCapacity = 12
	//   FixedGrowthFactor = 1.0
	//   fixed step = 12 * 1.0 = 12
	//
	//   → Pool grows: 48 → 60 → 72 → ...
	fixedGrowthFactor float64
}
