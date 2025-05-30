package test

import (
	"testing"
	"time"

	"github.com/AlexsanderHamir/PoolX/v2/pool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blocking mode is disabled by default, we always attempt to refill / grow,
// before hitting the retrive functions from the ring buffer, which will return nil in non-blocking mode.
func TestPoolGrowth(t *testing.T) {
	config, err := pool.NewPoolConfigBuilder[*TestObject]().
		SetInitialCapacity(2).
		SetGrowthFactor(50).
		SetFixedGrowthFactor(100).
		SetMinShrinkCapacity(2).
		SetShrinkCheckInterval(1 * time.Second).
		Build()
	require.NoError(t, err)

	allocator := func() *TestObject {
		return &TestObject{Value: 42}
	}

	cleaner := func(obj *TestObject) {
		obj.Value = 0
	}

	cloneTemplate := func(obj *TestObject) *TestObject {
		dst := *obj
		return &dst
	}

	p, err := pool.NewPool(config, allocator, cleaner, cloneTemplate)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, p.Close())
	}()

	poolObj := p.(*pool.Pool[*TestObject])

	objects := make([]*TestObject, 10)
	for i := range 10 {
		objects[i], err = p.Get()
		assert.NoError(t, err)
		assert.NotNil(t, objects[i])
	}

	assert.True(t, poolObj.IsGrowth())

	for _, obj := range objects {
		p.Put(obj)
	}
}

func TestPoolShrink(t *testing.T) {
	config, err := pool.NewPoolConfigBuilder[*TestObject]().
		SetInitialCapacity(32).
		EnforceCustomConfig().
		SetShrinkCheckInterval(10*time.Millisecond). // Very frequent checks
		SetShrinkCooldown(10*time.Millisecond).      // Short cooldown
		SetMinUtilizationBeforeShrink(90).           // Shrink if utilization below 10%
		SetStableUnderutilizationRounds(1).          // Only need 1 round of underutilization
		SetShrinkPercent(50).                        // Shrink by 50%
		SetMinShrinkCapacity(1).                     // Can shrink down to 1
		SetMaxConsecutiveShrinks(5).                 // Allow multiple consecutive shrinks
		SetFastPathBasicConfigs(32, 1, 1, 100, 20).
		Build()
	require.NoError(t, err)

	allocator := func() *TestObject {
		return &TestObject{Value: 42}
	}

	cleaner := func(obj *TestObject) {
		obj.Value = 0
	}

	cloneTemplate := func(obj *TestObject) *TestObject {
		dst := *obj
		return &dst
	}

	p, err := pool.NewPool(config, allocator, cleaner, cloneTemplate)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, p.Close())
	}()

	objects := make([]*TestObject, 10)
	for i := range 10 {
		objects[i], err = p.Get()
		assert.NoError(t, err)
		assert.NotNil(t, objects[i])
	}

	time.Sleep(300 * time.Millisecond)
	poolObj := p.(*pool.Pool[*TestObject])

	assert.True(t, poolObj.IsShrunk())

	for _, obj := range objects {
		p.Put(obj)
	}
}

func TestHardLimit(t *testing.T) {
	t.Run("non-blocking", func(t *testing.T) {
		config := createHardLimitTestConfig(t, false)
		p := createTestPool(t, config)
		defer func() {
			require.NoError(t, p.Close())
		}()
		runNilReturnTest(t, p)
	})

	t.Run("blocking", func(t *testing.T) {
		config := createHardLimitTestConfig(t, true)
		p := createTestPool(t, config)
		defer func() {
			require.NoError(t, p.Close())
		}()
		runBlockingTest(t, p)
	})

	t.Run("blocking-concurrent", func(t *testing.T) {
		config := createHardLimitTestConfig(t, true)
		p := createTestPool(t, config)
		defer func() {
			require.NoError(t, p.Close())
		}()
		runConcurrentBlockingTest(t, p, 20)
	})
}

func TestConfigValues(t *testing.T) {
	defaultConfig, err := pool.NewPoolConfigBuilder[*TestObject]().Build()
	require.NoError(t, err)

	originalValues := storeDefaultConfigValues(defaultConfig)

	customConfig := createCustomConfig(t)

	verifyCustomValuesDifferent(t, originalValues, customConfig)
}

func TestDisabledChannelGrowth(t *testing.T) {
	config, err := pool.NewPoolConfigBuilder[*TestObject]().
		SetInitialCapacity(2).
		SetGrowthFactor(50).
		SetFixedGrowthFactor(100).
		SetMinShrinkCapacity(2).
		SetFastPathEnableChannelGrowth(false). // Disable channel growth
		Build()
	require.NoError(t, err)

	allocator := func() *TestObject {
		return &TestObject{Value: 42}
	}

	cleaner := func(obj *TestObject) {
		obj.Value = 0
	}

	cloneTemplate := func(obj *TestObject) *TestObject {
		dst := *obj
		return &dst
	}

	p, err := pool.NewPool(config, allocator, cleaner, cloneTemplate)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, p.Close())
	}()

	// Verify initial state
	assert.False(t, config.GetFastPath().IsEnableChannelGrowth())

	// Get objects to trigger potential growth
	objects := make([]*TestObject, 10)
	for i := range 10 {
		objects[i], err = p.Get()
		assert.NoError(t, err)
		assert.NotNil(t, objects[i])
	}

	// Verify pool still grew despite channel growth being disabled
	poolObj := p.(*pool.Pool[*TestObject])
	assert.True(t, poolObj.IsRingBufferGrowth())
	assert.False(t, poolObj.IsFastPathGrowth())

	// Return objects
	for _, obj := range objects {
		p.Put(obj)
	}
}
