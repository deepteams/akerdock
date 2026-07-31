package instance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

type fakeSettingsStore struct {
	mu    sync.Mutex
	value store.InstanceSetting
	err   error
	calls int
}

func (f *fakeSettingsStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.value, f.err
}

func TestCacheRefreshReuseAndInvalidate(t *testing.T) {
	database := &fakeSettingsStore{value: store.InstanceSetting{Timezone: "Europe/Paris"}}
	cache := NewCache(database)

	first, err := cache.Get(context.Background())
	if err != nil || first.Timezone != "Europe/Paris" {
		t.Fatalf("first Get = %#v, %v", first, err)
	}
	database.value.Timezone = "UTC"
	second, err := cache.Get(context.Background())
	if err != nil || second.Timezone != "Europe/Paris" || database.calls != 1 {
		t.Fatalf("cached Get = %#v, %v, calls=%d", second, err, database.calls)
	}

	cache.Invalidate()
	third, err := cache.Get(context.Background())
	if err != nil || third.Timezone != "UTC" || database.calls != 2 {
		t.Fatalf("refreshed Get = %#v, %v, calls=%d", third, err, database.calls)
	}
}

func TestCacheDoesNotCacheReadFailure(t *testing.T) {
	database := &fakeSettingsStore{err: errors.New("database unavailable")}
	cache := NewCache(database)
	if _, err := cache.Get(context.Background()); err == nil {
		t.Fatal("database error was hidden")
	}
	database.err = nil
	database.value.Timezone = "UTC"
	got, err := cache.Get(context.Background())
	if err != nil || got.Timezone != "UTC" || database.calls != 2 {
		t.Fatalf("retry = %#v, %v, calls=%d", got, err, database.calls)
	}
}

func TestCacheServesStaleWhenRefreshFails(t *testing.T) {
	database := &fakeSettingsStore{value: store.InstanceSetting{Timezone: "Europe/Paris"}}
	cache := NewCache(database)
	if _, err := cache.Get(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	cache.Invalidate()
	database.err = errors.New("database unavailable")
	got, err := cache.Get(context.Background())
	if err != nil || got.Timezone != "Europe/Paris" {
		t.Fatalf("stale Get = %#v, %v — a primed cache must ride out a failing refresh", got, err)
	}

	database.err = nil
	database.value.Timezone = "UTC"
	cache.Invalidate()
	refreshed, err := cache.Get(context.Background())
	if err != nil || refreshed.Timezone != "UTC" {
		t.Fatalf("recovered Get = %#v, %v", refreshed, err)
	}
}

func TestCacheSerializesConcurrentRefresh(t *testing.T) {
	database := &fakeSettingsStore{value: store.InstanceSetting{Timezone: "UTC"}}
	cache := NewCache(database)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := cache.Get(context.Background()); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wait.Wait()
	if database.calls != 1 {
		t.Fatalf("concurrent refresh calls = %d, want 1", database.calls)
	}
}
