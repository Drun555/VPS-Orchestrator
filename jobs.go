package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type JobEvent struct {
	Sequence int64     `json:"sequence"`
	Type     string    `json:"type"`
	Message  string    `json:"message,omitempty"`
	Status   string    `json:"status,omitempty"`
	Time     time.Time `json:"time"`
}

type JobSnapshot struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	Events    []JobEvent `json:"events"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type Job struct {
	mu          sync.Mutex
	id          string
	kind        string
	status      string
	errorText   string
	events      []JobEvent
	sequence    int64
	createdAt   time.Time
	updatedAt   time.Time
	hasWarnings bool
	subscribers map[chan JobEvent]struct{}
	redactions  []string
}

type JobManager struct {
	mu    sync.RWMutex
	jobs  map[string]*Job
	order []string
}

type JobReporter struct {
	job *Job
}

func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[string]*Job)}
}

func (manager *JobManager) Start(kind string, redactions []string, work func(*JobReporter) error) (*Job, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	job := &Job{
		id: id, kind: kind, status: "running", createdAt: now, updatedAt: now,
		subscribers: make(map[chan JobEvent]struct{}), redactions: compactRedactions(redactions),
	}
	manager.mu.Lock()
	manager.jobs[id] = job
	manager.order = append(manager.order, id)
	if len(manager.order) > 100 {
		oldest := manager.order[0]
		manager.order = manager.order[1:]
		delete(manager.jobs, oldest)
	}
	manager.mu.Unlock()

	reporter := &JobReporter{job: job}
	go func() {
		reporter.Log("Задача запущена.")
		if err := work(reporter); err != nil {
			reporter.fail(err)
			return
		}
		reporter.success()
	}()
	return job, nil
}

func (manager *JobManager) Get(id string) (*Job, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	job, ok := manager.jobs[id]
	return job, ok
}

func (reporter *JobReporter) Log(format string, values ...any) {
	reporter.job.append(JobEvent{Type: "log", Message: fmt.Sprintf(format, values...), Time: time.Now().UTC()})
}

func (reporter *JobReporter) Warning(format string, values ...any) {
	reporter.job.append(JobEvent{Type: "warning", Message: fmt.Sprintf(format, values...), Time: time.Now().UTC()})
}

func (reporter *JobReporter) success() {
	reporter.job.mu.Lock()
	hasWarnings := reporter.job.hasWarnings
	reporter.job.mu.Unlock()
	if hasWarnings {
		reporter.job.finish("success_with_warnings", "")
		return
	}
	reporter.job.finish("success", "")
}

func (reporter *JobReporter) fail(err error) {
	reporter.job.finish("error", err.Error())
}

func (job *Job) append(event JobEvent) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.sequence++
	event.Sequence = job.sequence
	event.Message = job.redact(event.Message)
	if event.Type == "warning" {
		job.hasWarnings = true
	}
	job.events = append(job.events, event)
	if len(job.events) > 2000 {
		job.events = append([]JobEvent(nil), job.events[len(job.events)-2000:]...)
	}
	job.updatedAt = event.Time
	for subscriber := range job.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (job *Job) finish(status, errorText string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	now := time.Now().UTC()
	job.status = status
	job.errorText = job.redact(errorText)
	job.updatedAt = now
	job.sequence++
	event := JobEvent{Sequence: job.sequence, Type: "status", Status: status, Message: job.errorText, Time: now}
	job.events = append(job.events, event)
	for subscriber := range job.subscribers {
		select {
		case subscriber <- event:
		default:
		}
		close(subscriber)
		delete(job.subscribers, subscriber)
	}
}

func (job *Job) Snapshot() JobSnapshot {
	job.mu.Lock()
	defer job.mu.Unlock()
	return JobSnapshot{
		ID: job.id, Kind: job.kind, Status: job.status, Error: job.errorText,
		Events: append([]JobEvent(nil), job.events...), CreatedAt: job.createdAt, UpdatedAt: job.updatedAt,
	}
}

func (job *Job) Subscribe(after int64) ([]JobEvent, <-chan JobEvent, func()) {
	job.mu.Lock()
	defer job.mu.Unlock()
	past := make([]JobEvent, 0)
	for _, event := range job.events {
		if event.Sequence > after {
			past = append(past, event)
		}
	}
	channel := make(chan JobEvent, 64)
	if job.status == "running" {
		job.subscribers[channel] = struct{}{}
	} else {
		close(channel)
	}
	cancel := func() {
		job.mu.Lock()
		defer job.mu.Unlock()
		if _, exists := job.subscribers[channel]; exists {
			delete(job.subscribers, channel)
			close(channel)
		}
	}
	return past, channel, cancel
}

func (job *Job) redact(value string) string {
	for _, secret := range job.redactions {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func compactRedactions(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, value := range values {
		if len(value) < 4 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func marshalSSE(event JobEvent) []byte {
	data, _ := json.Marshal(event)
	return data
}

func (reporter *JobReporter) Redact(secrets ...string) {
	reporter.job.mu.Lock()
	defer reporter.job.mu.Unlock()
	reporter.job.redactions = compactRedactions(append(reporter.job.redactions, secrets...))
}
func (reporter *JobReporter) SafeError(err error) string {
	reporter.job.mu.Lock()
	defer reporter.job.mu.Unlock()
	return reporter.job.redact(err.Error())
}
