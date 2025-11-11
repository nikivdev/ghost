package main

import (
	"sync"
)

type ServerManager struct {
	mu   sync.Mutex
	jobs []*serverJob
}

func (m *ServerManager) Apply(servers []NormalizedServer) {
	oldJobs := m.swapJobs(nil)
	for _, job := range oldJobs {
		if job == nil {
			continue
		}
		if err := job.Close(); err != nil {
			logError("failed to stop server: %v", err)
		}
	}

	newJobs := make([]*serverJob, 0, len(servers))
	for _, cfg := range servers {
		job, err := newServerJob(cfg)
		if err != nil {
			logError("failed to start server %q: %v", cfg.Name, err)
			continue
		}
		newJobs = append(newJobs, job)
	}

	m.swapJobs(newJobs)
	logInfo("loaded %d server(s)", len(newJobs))
}

func (m *ServerManager) StopAll() {
	jobs := m.swapJobs(nil)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if err := job.Close(); err != nil {
			logError("failed to stop server: %v", err)
		}
	}
}

func (m *ServerManager) swapJobs(jobs []*serverJob) []*serverJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.jobs
	m.jobs = jobs
	return old
}
