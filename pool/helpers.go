package pool

import (
	"fmt"
	"log"
	"time"
)

func (p *pool[T]) updateAvailableObjs() {
	p.mu.RLock()
	p.stats.availableObjects.Store(uint64(p.pool.Length() + len(p.cacheL1)))
	p.mu.RUnlock()
}

// better name this function
func (p *pool[T]) releaseObj(obj T) {
	p.cleaner(obj)

	for {
		old := p.stats.objectsInUse.Load()
		if old == 0 {
			break
		}

		if p.stats.objectsInUse.CompareAndSwap(old, old-1) {
			break
		}
	}
}

func (p *pool[T]) reduceL1Hit() {
	for {
		old := p.stats.l1HitCount.Load()
		if old == 0 {
			break
		}

		if p.stats.l1HitCount.CompareAndSwap(old, old-1) {
			break
		}
	}
}

func (p *pool[T]) reduceBlockedGets() {
	for {
		old := p.stats.blockedGets.Load()
		if old == 0 {
			break
		}

		if p.stats.blockedGets.CompareAndSwap(old, old-1) {
			break
		}
	}
}

func (p *pool[T]) calculateNewPoolCapacity(currentCap, threshold, fixedStep uint64, cfg *growthParameters) uint64 {
	if currentCap < threshold {
		growth := maxUint64(uint64(float64(currentCap)*cfg.growthPercent), 1)
		newCap := currentCap + growth
		if p.config.verbose {
			log.Printf("[GROW] Strategy: exponential | Threshold: %d | Current: %d | Growth: %d | New capacity: %d", threshold, currentCap, growth, newCap)
		}
		return newCap
	}
	newCap := currentCap + fixedStep
	if p.config.verbose {
		log.Printf("[GROW] Strategy: fixed-step | Threshold: %d | Current: %d | Step: %d | New capacity: %d",
			threshold, currentCap, fixedStep, newCap)
	}
	return newCap
}

func (p *pool[T]) growthWouldExceedHardLimit(newCapacity uint64) bool {
	return newCapacity > uint64(p.config.hardLimit) && p.stats.currentCapacity.Load() >= uint64(p.config.hardLimit)
}

func (p *pool[T]) needsToShrinkToHardLimit(currentCap, newCapacity uint64) bool {
	return currentCap < uint64(p.config.hardLimit) && newCapacity > uint64(p.config.hardLimit)
}

func (p *pool[T]) tryL1ResizeIfTriggered() {
	trigger := uint64(p.config.fastPath.growthEventsTrigger)
	if !p.config.fastPath.enableChannelGrowth {
		return
	}

	sinceLastResize := p.stats.totalGrowthEvents.Load() - p.stats.lastResizeAtGrowthNum.Load()
	if sinceLastResize < trigger {
		return
	}

	cfg := p.config.fastPath.growth
	currentCap := p.stats.currentL1Capacity.Load()
	threshold := uint64(float64(currentCap) * cfg.exponentialThresholdFactor)
	step := uint64(float64(currentCap) * cfg.fixedGrowthFactor)

	var newCap uint64
	if currentCap < threshold {
		newCap = currentCap + maxUint64(uint64(float64(currentCap)*cfg.growthPercent), 1)
	} else {
		newCap = currentCap + step
	}

	newL1 := make(chan T, newCap)
	for {
		select {
		case obj := <-p.cacheL1:
			newL1 <- obj
		default:
			goto done
		}
	}
done:

	p.cacheL1 = newL1
	p.stats.currentL1Capacity.Store(newCap)
	p.stats.lastResizeAtGrowthNum.Store(p.stats.totalGrowthEvents.Load())
}

func (p *pool[T]) PrintPoolStats() {
	totalAvailable := p.pool.Length() + len(p.cacheL1)
	log.Printf("[STATS] Total available objects: %d", totalAvailable)
	log.Printf("[STATS] Ring buffer length: %d", p.pool.Length())
	log.Printf("[STATS] L1 cache length: %d", len(p.cacheL1))
}

func getShrinkDefaultsMap() map[AggressivenessLevel]*shrinkDefaults {
	return defaultShrinkMap
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func (p *pool[T]) setPoolAndBuffer(obj T, fastPathRemaining int) int {
	if fastPathRemaining > 0 {
		p.cacheL1 <- obj
		fastPathRemaining--
	} else {
		if err := p.pool.Write(obj); err != nil {
			if p.config.verbose {
				log.Printf("[SETPOOL] Error writing to ring buffer: %v", err)
			}
		}
	}
	return fastPathRemaining
}

func (p *pool[T]) IndleCheck(idles *int, shrinkPermissionIdleness *bool) {
	idleDuration := time.Since(p.stats.lastTimeCalledGet)
	threshold := p.config.shrink.idleThreshold
	required := p.config.shrink.minIdleBeforeShrink

	if idleDuration >= threshold {
		*idles += 1
		if *idles >= required {
			*shrinkPermissionIdleness = true
		}
		if p.config.verbose {
			log.Printf("[SHRINK] IdleCheck passed — idleDuration: %v | idles: %d/%d", idleDuration, *idles, required)
		}
	} else {
		if *idles > 0 {
			*idles -= 1
		}
		if p.config.verbose {
			log.Printf("[SHRINK] IdleCheck reset — recent activity detected | idles: %d/%d", *idles, required)
		}
	}
}

func (p *pool[T]) UtilizationCheck(underutilizationRounds *int, shrinkPermissionUtilization *bool) {
	p.updateAvailableObjs()

	inUse := p.stats.objectsInUse.Load()
	available := p.stats.availableObjects.Load()
	total := inUse + available

	var utilization float64
	if total > 0 {
		utilization = (float64(inUse) / float64(total)) * 100
	}

	minUtil := p.config.shrink.minUtilizationBeforeShrink
	requiredRounds := p.config.shrink.stableUnderutilizationRounds

	if utilization <= minUtil {
		*underutilizationRounds += 1
		if p.config.verbose {
			log.Printf("[SHRINK] UtilizationCheck — utilization: %.2f%% (threshold: %.2f%%) | round: %d/%d",
				utilization, minUtil, *underutilizationRounds, requiredRounds)
		}

		if *underutilizationRounds >= requiredRounds {
			*shrinkPermissionUtilization = true
			if p.config.verbose {
				log.Println("[SHRINK] UtilizationCheck — underutilization stable, shrink allowed")
			}
		}
	} else {
		if *underutilizationRounds > 0 {
			*underutilizationRounds -= 1
			if p.config.verbose {
				log.Printf("[SHRINK] UtilizationCheck — usage recovered, reducing rounds: %d/%d",
					*underutilizationRounds, requiredRounds)
			}
		} else {
			if p.config.verbose {
				log.Printf("[SHRINK] UtilizationCheck — usage healthy: %.2f%% > %.2f%%", utilization, minUtil)
			}
		}
	}
}

func (p *pool[T]) updateUsageStats() {
	currentInUse := p.stats.objectsInUse.Add(1)
	p.stats.totalGets.Add(1)

	for {
		peak := p.stats.peakInUse.Load()
		if currentInUse <= peak {
			break
		}

		if p.stats.peakInUse.CompareAndSwap(peak, currentInUse) {
			break
		}
	}

	for {
		current := p.stats.consecutiveShrinks.Load()
		if current == 0 {
			break
		}
		if p.stats.consecutiveShrinks.CompareAndSwap(current, current-1) {
			break
		}
	}
}

func (p *shrinkParameters) ApplyDefaults(table map[AggressivenessLevel]*shrinkDefaults) {
	if p.aggressivenessLevel < AggressivenessDisabled {
		p.aggressivenessLevel = AggressivenessDisabled
	}

	if p.aggressivenessLevel > AggressivenessExtreme {
		p.aggressivenessLevel = AggressivenessExtreme
	}

	def, ok := table[p.aggressivenessLevel]
	if !ok {
		return
	}

	p.checkInterval = def.interval
	p.idleThreshold = def.idle
	p.minIdleBeforeShrink = def.minIdle
	p.shrinkCooldown = def.cooldown
	p.minUtilizationBeforeShrink = def.utilization
	p.stableUnderutilizationRounds = def.underutilized
	p.shrinkPercent = def.percent
	p.maxConsecutiveShrinks = def.maxShrinks
	p.minCapacity = defaultMinCapacity
}

func (p *pool[T]) refill(fillTarget int) RefillResult {
	var result RefillResult

	noObjsAvailable := p.pool.Length() == 0
	biggerRequestThanAvailable := fillTarget > p.pool.Length() // WARNING - reading more than available objects will block.

	if noObjsAvailable || biggerRequestThanAvailable {
		if p.isGrowthBlocked {
			result.Reason = GrowthBlocked
			result.GrowthBlocked = true
			return result
		}

		now := time.Now()
		if !p.grow(now) {
			result.Reason = GrowthFailed
			return result
		}
		result.GrowthNeeded = true
	}

	currentObjsAvailable := p.pool.Length()
	toMove := min(fillTarget, currentObjsAvailable)

	items, err := p.pool.GetN(toMove)
	if err != nil && err != ErrIsEmpty {
		if p.config.verbose {
			log.Printf("[REFILL] Error getting items from ring buffer: %v", err)
		}
		result.Reason = fmt.Sprintf("%s: %v", RingBufferError, err)
		return result
	}

	if items == nil && err == nil {
		if p.config.verbose {
			log.Println("[REFILL] No items to move. (fillTarget == 0 || currentObjsAvailable == 0)")
		}
		result.Reason = NoItemsToMove
		return result
	}

	for _, item := range items {
		select {
		case p.cacheL1 <- item:
			result.ItemsMoved++
			if p.config.verbose {
				log.Printf("[REFILL] Moved object to L1 | Remaining: %d", p.pool.Length())
			}
		default:
			result.ItemsFailed++
			if err := p.pool.Write(item); err != nil {
				if p.config.verbose {
					log.Printf("[REFILL] Error putting object back in ring buffer: %v", err)
				}
			}
		}
	}

	result.Success = true
	result.Reason = RefillSucceeded
	return result
}

func (p *pool[T]) ShrinkExecution() {
	p.logShrinkHeader()

	currentCap := p.stats.currentCapacity.Load()
	inUse := int(p.stats.objectsInUse.Load())
	newCapacity := int(float64(currentCap) * (1.0 - p.config.shrink.shrinkPercent))

	if !p.shouldShrinkMainPool(currentCap, newCapacity, inUse) {
		return
	}

	newCapacity = p.adjustMainShrinkTarget(newCapacity, inUse)

	p.performShrink(newCapacity, inUse, currentCap)
	if p.config.verbose {
		log.Printf("[SHRINK] Shrink complete — Final capacity: %d", newCapacity)
		log.Println("[SHRINK] ----------------------------------------")
	}

	// Fast Path (L1)
	if !p.config.fastPath.enableChannelGrowth || !p.shouldShrinkFastPath() {
		return
	}

	currentCap = p.stats.currentL1Capacity.Load()
	newCapacity = p.adjustFastPathShrinkTarget(currentCap)

	p.logFastPathShrink(currentCap, newCapacity, inUse)
	p.shrinkFastPath(newCapacity, inUse)

	if p.config.verbose {
		log.Printf("[SHRINK | FAST PATH] Shrink complete — Final capacity: %d", newCapacity)
		log.Println("[SHRINK | FAST PATH] ----------------------------------------")
	}

	if p.config.verbose {
		log.Printf("[SHRINK] Final state | New capacity: %d | Ring buffer length: %d", newCapacity, p.pool.Length())
	}
}

func (p *pool[T]) performShrink(newCapacity, inUse int, currentCap uint64) {
	availableToKeep := newCapacity - inUse
	if availableToKeep <= 0 {
		if p.config.verbose {
			log.Printf("[SHRINK] Skipped — no room for available objects after shrink (in-use: %d, requested: %d)", inUse, newCapacity)
		}
		return
	}

	newRingBuffer := New[T](newCapacity)
	newRingBuffer.CopyConfig(p.pool)

	itemsToKeep := min(availableToKeep, p.pool.Length())
	if itemsToKeep > 0 {
		items, err := p.pool.GetN(itemsToKeep)
		if err != nil && err != ErrIsEmpty {
			if p.config.verbose {
				log.Printf("[SHRINK] Error getting items from old ring buffer: %v", err)
			}
			return
		}

		if _, err := newRingBuffer.WriteMany(items); err != nil {
			if p.config.verbose {
				log.Printf("[SHRINK] Error writing items to new ring buffer: %v", err)
			}
			return
		}
	}

	p.pool.ClearRemaining()
	p.pool = newRingBuffer

	p.stats.currentCapacity.Store(uint64(newCapacity))
	p.stats.totalShrinkEvents.Add(1)
	p.stats.lastShrinkTime = time.Now()
	p.stats.consecutiveShrinks.Add(1)

	if p.config.verbose {
		log.Printf("[SHRINK] Shrinking pool → From: %d → To: %d | Preserved: %d | In-use: %d",
			currentCap, newCapacity, itemsToKeep, inUse)
	}
}

func (p *pool[T]) logShrinkHeader() {
	log.Println("[SHRINK] ----------------------------------------")
	log.Println("[SHRINK] Starting shrink execution")
}

func (p *pool[T]) shouldShrinkMainPool(currentCap uint64, newCap int, inUse int) bool {
	minCap := p.config.shrink.minCapacity

	if p.config.verbose {
		log.Printf("[SHRINK] Current capacity       : %d", currentCap)
		log.Printf("[SHRINK] Requested new capacity : %d", newCap)
		log.Printf("[SHRINK] Minimum allowed        : %d", minCap)
		log.Printf("[SHRINK] Currently in use       : %d", inUse)
		log.Printf("[SHRINK] Pool length            : %d", p.pool.Length())
	}

	switch {
	case newCap == 0:
		if p.config.verbose {
			log.Println("[SHRINK] Skipped — new capacity is zero (invalid)")
		}
		return false
	case currentCap <= uint64(minCap):
		if p.config.verbose {
			log.Printf("[SHRINK] Skipped — current capacity (%d) is at or below MinCapacity (%d)", currentCap, minCap)
		}
		return false
	case newCap >= int(currentCap):
		if p.config.verbose {
			log.Printf("[SHRINK] Skipped — new capacity (%d) is not smaller than current (%d)", newCap, currentCap)
		}
		return false
	}

	totalAvailable := p.pool.Length() + len(p.cacheL1)
	if totalAvailable == 0 {
		if p.config.verbose {
			log.Printf("[SHRINK] Skipped — all %d objects are currently in use, no shrink possible", inUse)
		}
		return false
	}

	return true
}

func (p *pool[T]) adjustMainShrinkTarget(newCap, inUse int) int {
	minCap := p.config.shrink.minCapacity

	if newCap < minCap {
		if p.config.verbose {
			log.Printf("[SHRINK] Adjusting to min capacity: %d", minCap)
		}
		newCap = minCap
	}
	if newCap < inUse {
		if p.config.verbose {
			log.Printf("[SHRINK] Adjusting to match in-use objects: %d", inUse)
		}
		newCap = inUse
	}

	if newCap < p.config.hardLimit {
		if p.config.verbose {
			log.Printf("[SHRINK] Allowing growth, capacity is lower than hard limit: %d", p.config.hardLimit)
		}
		p.isGrowthBlocked = false
	}
	return newCap
}

func (p *pool[T]) shouldShrinkFastPath() bool {
	sinceLast := p.stats.totalShrinkEvents.Load() - p.stats.lastResizeAtShrinkNum.Load()
	trigger := uint64(p.config.fastPath.shrinkEventsTrigger)

	return sinceLast >= trigger
}

func (p *pool[T]) adjustFastPathShrinkTarget(currentCap uint64) int {
	cfg := p.config.fastPath.shrink
	newCap := int(float64(currentCap) * (1.0 - cfg.shrinkPercent))

	if newCap < cfg.minCapacity {
		if p.config.verbose {
			log.Printf("[SHRINK | FAST PATH] Adjusting to min capacity: %d", cfg.minCapacity)
		}
		newCap = cfg.minCapacity
	}
	return newCap
}

func (p *pool[T]) shrinkFastPath(newCapacity, inUse int) {
	availableObjsToCopy := newCapacity - inUse
	if availableObjsToCopy <= 0 {
		if p.config.verbose {
			log.Printf("[SHRINK] Skipped — no room for available objects after shrink (in-use: %d, requested: %d)", inUse, newCapacity)
		}
		return
	}

	copyCount := min(availableObjsToCopy, len(p.cacheL1))
	newL1 := make(chan T, newCapacity) // WARNING - expensive operation.

	for range copyCount {
		select {
		// WARNING - if the cacheL1 is empty, this is fine, but if newL1 is full, this will block, and we're dropping objects.
		case obj := <-p.cacheL1:
			newL1 <- obj
		default:
			if p.config.verbose {
				log.Println("[SHRINK] - cacheL1 is empty, or newL1 is full")
			}
		}
	}

	p.cacheL1 = newL1
	p.stats.lastResizeAtShrinkNum.Store(p.stats.totalShrinkEvents.Load())
}

func (p *pool[T]) logFastPathShrink(currentCap uint64, newCap int, inUse int) {
	if p.config.verbose {
		log.Println("[SHRINK | FAST PATH] ----------------------------------------")
		log.Printf("[SHRINK | FAST PATH] Starting shrink execution")
		log.Printf("[SHRINK | FAST PATH] Current L1 capacity     : %d", currentCap)
		log.Printf("[SHRINK | FAST PATH] Requested new capacity  : %d", newCap)
		log.Printf("[SHRINK | FAST PATH] Minimum allowed         : %d", p.config.fastPath.shrink.minCapacity)
		log.Printf("[SHRINK | FAST PATH] Currently in use        : %d", inUse)
		log.Printf("[SHRINK | FAST PATH] Channel length (cached) : %d", len(p.cacheL1))
	}
}

// Set default fields
func InitDefaultFields() {
	defaultFastPath.shrink.minCapacity = defaultL1MinCapacity
	defaultShrinkParameters.ApplyDefaults(getShrinkDefaultsMap())
}

func (p *pool[T]) updateDerivedStats() {
	totalGets := p.stats.totalGets.Load()
	objectsInUse := p.stats.objectsInUse.Load()
	availableObjects := p.stats.availableObjects.Load()

	currentCapacity := p.stats.currentCapacity.Load()

	initialCapacity := p.stats.initialCapacity
	totalCreated := currentCapacity - uint64(initialCapacity)

	p.stats.mu.Lock()
	if totalCreated > 0 {
		p.stats.reqPerObj = float64(totalGets) / float64(totalCreated)
	}

	// WARNING -  at the time calculated, it may be low, be selective.
	totalObjects := objectsInUse + availableObjects
	if totalObjects > 0 {
		p.stats.utilization = (float64(objectsInUse) / float64(totalObjects)) * 100
	}

	p.stats.mu.Unlock()
}
