package provider

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Provider interface {
	Send(ctx context.Context, phone, body string) (providerID string, err error)
}

type TransientError struct{ msg string }

func (e *TransientError) Error() string { return e.msg }

type LatencyStep struct {
	At      time.Duration
	Latency time.Duration
}

func ParseSchedule(s string) ([]LatencyStep, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var steps []LatencyStep
	for _, part := range strings.Split(s, ",") {
		at, lat, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("bad schedule step %q (want AT:LATENCY)", part)
		}
		atD, err := time.ParseDuration(at)
		if err != nil {
			return nil, fmt.Errorf("bad step time %q: %w", at, err)
		}
		latD, err := time.ParseDuration(lat)
		if err != nil {
			return nil, fmt.Errorf("bad step latency %q: %w", lat, err)
		}
		steps = append(steps, LatencyStep{At: atD, Latency: latD})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].At < steps[j].At })
	return steps, nil
}

type Mock struct {
	Latency     time.Duration 
	FailureRate float64       
	Schedule    []LatencyStep 

	start time.Time
	once  sync.Once
}

func (m *Mock) currentLatency() time.Duration {
	if len(m.Schedule) == 0 {
		return m.Latency
	}
	m.once.Do(func() { m.start = time.Now() })
	elapsed := time.Since(m.start)
	lat := m.Latency
	for _, s := range m.Schedule {
		if elapsed >= s.At {
			lat = s.Latency
		} else {
			break
		}
	}
	return lat
}

func (m *Mock) Send(ctx context.Context, phone, body string) (string, error) {
	select {
	case <-time.After(m.currentLatency()):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if rand.Float64() < m.FailureRate {
		return "", &TransientError{msg: "operator temporarily unavailable"}
	}
	return "op-" + uuid.NewString(), nil
}
