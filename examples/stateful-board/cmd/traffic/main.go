package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type actor struct {
	email, token string
	posts        []int64
}
type counters struct {
	requests, succeeded, failed atomic.Int64
	mu                          sync.Mutex
	latency                     []time.Duration
	statuses                    map[int]int64
	operations                  map[string]int64
}

func main() {
	target := flag.String("target", "", "public application URL")
	duration := flag.Duration("duration", 3*time.Minute, "traffic duration")
	users := flag.Int("users", 32, "concurrent user sessions")
	flag.Parse()
	if *target == "" {
		fmt.Fprintln(os.Stderr, "--target is required")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256}}
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	actors := make([]*actor, *users)
	stats := &counters{statuses: map[int]int64{}, operations: map[string]int64{}}
	for i := range actors {
		actors[i] = signup(client, strings.TrimRight(*target, "/"), runID, i, stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for i, a := range actors {
		wg.Add(1)
		go func(index int, a *actor) {
			defer wg.Done()
			exercise(ctx, client, strings.TrimRight(*target, "/"), index, a, stats)
		}(i, a)
	}
	ticker := time.NewTicker(15 * time.Second)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-ticker.C:
			report(stats, time.Since(start), false)
		case <-done:
			ticker.Stop()
			report(stats, time.Since(start), true)
			return
		}
	}
}

func signup(client *http.Client, target, runID string, index int, stats *counters) *actor {
	email := fmt.Sprintf("user-%s-%03d@example.test", runID, index)
	var result struct {
		Token string `json:"token"`
	}
	status, body, _ := request(client, "POST", target+"/api/users", "", map[string]string{"email": email, "displayName": fmt.Sprintf("User %03d", index)}, stats, "signup")
	if status != 201 || json.Unmarshal(body, &result) != nil || result.Token == "" {
		panic(fmt.Sprintf("signup failed status=%d body=%s", status, body))
	}
	return &actor{email: email, token: result.Token}
}

func exercise(ctx context.Context, client *http.Client, target string, index int, a *actor, stats *counters) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(index)))
	for ctx.Err() == nil {
		choice := rng.Intn(100)
		switch {
		case choice < 48:
			request(client, "GET", target+"/api/feed", a.token, nil, stats, "feed")
		case choice < 68:
			body := map[string]string{"body": fmt.Sprintf("Board update %d from user %d about design, reliability, and shipping.", rng.Int63(), index)}
			status, payload, _ := request(client, "POST", target+"/api/posts", a.token, body, stats, "create_post")
			if status == 201 {
				var p struct {
					ID int64 `json:"id"`
				}
				if json.Unmarshal(payload, &p) == nil {
					a.posts = append(a.posts, p.ID)
				}
			}
		case choice < 77 && len(a.posts) > 0:
			id := a.posts[rng.Intn(len(a.posts))]
			request(client, "PATCH", fmt.Sprintf("%s/api/posts/%d", target, id), a.token, map[string]string{"body": fmt.Sprintf("Revised board update %d with concrete decisions.", rng.Int63())}, stats, "update_post")
		case choice < 86:
			query := []string{"board", "design", "reliability", "shipping"}[rng.Intn(4)]
			request(client, "GET", target+"/api/search?q="+url.QueryEscape(query), a.token, nil, stats, "search")
		case choice < 93:
			request(client, "GET", target+"/api/me", a.token, nil, stats, "me")
		case choice < 98:
			status, payload, _ := request(client, "POST", target+"/api/sessions", "", map[string]string{"email": a.email}, stats, "login")
			if status == 201 {
				var session struct {
					Token string `json:"token"`
				}
				if json.Unmarshal(payload, &session) == nil && session.Token != "" {
					a.token = session.Token
				}
			}
		default:
			if len(a.posts) > 0 {
				id := a.posts[rng.Intn(len(a.posts))]
				request(client, "POST", fmt.Sprintf("%s/api/posts/%d/like", target, id), a.token, map[string]bool{}, stats, "like")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(40+rng.Intn(211)) * time.Millisecond):
		}
	}
}

func request(client *http.Client, method, target, token string, input any, stats *counters, operation string) (int, []byte, error) {
	var body io.Reader
	if input != nil {
		payload, _ := json.Marshal(input)
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return 0, nil, err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started)
	stats.requests.Add(1)
	stats.mu.Lock()
	stats.latency = append(stats.latency, latency)
	stats.operations[operation]++
	stats.mu.Unlock()
	if err != nil {
		stats.failed.Add(1)
		return 0, nil, err
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	stats.mu.Lock()
	stats.statuses[resp.StatusCode]++
	stats.mu.Unlock()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		stats.succeeded.Add(1)
	} else {
		stats.failed.Add(1)
	}
	return resp.StatusCode, payload, nil
}

func report(stats *counters, elapsed time.Duration, final bool) {
	stats.mu.Lock()
	samples := append([]time.Duration(nil), stats.latency...)
	statuses := clone(stats.statuses)
	operations := clone(stats.operations)
	stats.mu.Unlock()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	percentile := func(p float64) int64 {
		if len(samples) == 0 {
			return 0
		}
		return samples[int(float64(len(samples)-1)*p)].Milliseconds()
	}
	result := map[string]any{"final": final, "elapsedSeconds": elapsed.Seconds(), "requests": stats.requests.Load(), "succeeded": stats.succeeded.Load(), "failed": stats.failed.Load(), "requestsPerSecond": float64(stats.requests.Load()) / elapsed.Seconds(), "latencyMs": map[string]int64{"p50": percentile(.50), "p95": percentile(.95), "p99": percentile(.99)}, "statuses": statuses, "operations": operations}
	payload, _ := json.Marshal(result)
	fmt.Println(string(payload))
}
func clone[K comparable](source map[K]int64) map[K]int64 {
	target := make(map[K]int64, len(source))
	for k, v := range source {
		target[k] = v
	}
	return target
}
