package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rhobs/rhobs-synthetics-agent/internal/api"
)

// RealAPIManager manages the lifecycle of the actual RHOBS Synthetics API server
type RealAPIManager struct {
	cmd         *exec.Cmd
	apiURL      string
	port        int
	dataDir     string
	apiPath     string
	stopChan    chan struct{}
	started     bool
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewRealAPIManager creates a new manager for the real API server
func NewRealAPIManager() *RealAPIManager {
	ctx, cancel := context.WithCancel(context.Background())

	// Get API path from environment variable, fallback to default
	apiPath := os.Getenv("RHOBS_SYNTHETICS_API_PATH")
	if apiPath == "" {
		apiPath = "../rhobs-synthetics-api"
	}

	return &RealAPIManager{
		port:     8080,
		dataDir:  "/tmp/rhobs-synthetics-api-test-data",
		apiPath:  apiPath,
		stopChan: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start builds and starts the real RHOBS Synthetics API server
func (m *RealAPIManager) Start() error {
	if m.started {
		return fmt.Errorf("API server already started")
	}

	// Build the API server first
	if err := m.buildAPI(); err != nil {
		return fmt.Errorf("failed to build API: %w", err)
	}

	// Create data directory
	if err := os.MkdirAll(m.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Find an available port
	if err := m.findAvailablePort(); err != nil {
		return fmt.Errorf("failed to find available port: %w", err)
	}

	m.apiURL = fmt.Sprintf("http://localhost:%d", m.port)

	// Start the API server
	if err := m.startAPI(); err != nil {
		return fmt.Errorf("failed to start API: %w", err)
	}

	// Wait for API to be ready
	if err := m.waitForAPI(); err != nil {
		_ = m.Stop()
		return fmt.Errorf("API failed to become ready: %w", err)
	}

	// Seed with test data
	if err := m.SeedTestData(); err != nil {
		_ = m.Stop()
		return fmt.Errorf("failed to seed test data: %w", err)
	}

	m.started = true
	return nil
}

// Stop shuts down the API server
func (m *RealAPIManager) Stop() error {
	if !m.started {
		return nil
	}

	m.cancel()
	close(m.stopChan)

	if m.cmd != nil && m.cmd.Process != nil {
		// Try graceful shutdown first
		if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// Force kill if graceful shutdown fails
			_ = m.cmd.Process.Kill()
		}
		_ = m.cmd.Wait()
	}

	// Clean up data directory
	_ = os.RemoveAll(m.dataDir)

	m.started = false
	return nil
}

// GetURL returns the API server URL
func (m *RealAPIManager) GetURL() string {
	return m.apiURL
}

// buildAPI builds the RHOBS Synthetics API binary
func (m *RealAPIManager) buildAPI() error {
	cmd := exec.CommandContext(m.ctx, "make", "build")
	cmd.Dir = m.apiPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build API: %w", err)
	}

	return nil
}

// findAvailablePort finds an available port starting from 8080
func (m *RealAPIManager) findAvailablePort() error {
	for port := 8080; port < 8100; port++ {
		if m.isPortAvailable(port) {
			m.port = port
			return nil
		}
	}
	return fmt.Errorf("no available ports found")
}

// isPortAvailable checks if a port is available
func (m *RealAPIManager) isPortAvailable(port int) bool {
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return true // Port is available
	}
	_ = conn.Close()
	return false // Port is in use
}

// startAPI starts the API server process
func (m *RealAPIManager) startAPI() error {
	binaryPath := filepath.Join(m.apiPath, "rhobs-synthetics-api")

	m.cmd = exec.CommandContext(m.ctx, binaryPath,
		"start",
		"--database-engine", "local",
		"--data-dir", m.dataDir,
		"--port", strconv.Itoa(m.port),
		"--log-level", "debug",
		"--graceful-timeout", "5s",
	)

	// Set up environment for development mode
	m.cmd.Env = append(os.Environ(), "APP_ENV=dev")

	// Capture output for debugging
	stdout, err := m.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := m.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start API process: %w", err)
	}

	// Start goroutines to handle output
	go m.handleOutput("stdout", stdout)
	go m.handleOutput("stderr", stderr)

	return nil
}

// handleOutput handles stdout/stderr from the API process
func (m *RealAPIManager) handleOutput(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-m.stopChan:
			return
		default:
			// Only print API output if we want to debug
			// fmt.Printf("[API %s] %s\n", prefix, scanner.Text())
		}
	}
}

// waitForAPI waits for the API server to become ready
func (m *RealAPIManager) waitForAPI() error {
	client := &http.Client{Timeout: 1 * time.Second}

	for i := 0; i < 30; i++ { // Wait up to 30 seconds
		select {
		case <-m.stopChan:
			return fmt.Errorf("API startup cancelled")
		default:
		}

		resp, err := client.Get(m.apiURL + "/readyz")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("API did not become ready within timeout")
}

// SeedTestData creates test probes in the API
func (m *RealAPIManager) SeedTestData() error {
	// Create test probes
	testProbes := []struct {
		staticURL string
		labels    map[string]string
	}{
		{
			staticURL: "https://httpbin.org/status/200",
			labels: map[string]string{
				"env":     "test",
				"private": "false",
				"rhobs-synthetics/status": "pending",
			},
		},
		{
			staticURL: "https://httpbin.org/get",
			labels: map[string]string{
				"env":     "test",
				"private": "false",
				"rhobs-synthetics/status": "pending",
			},
		},
	}

	for _, testProbe := range testProbes {
		probeData := map[string]interface{}{
			"static_url": testProbe.staticURL,
			"labels":     testProbe.labels,
		}

		jsonData, err := json.Marshal(probeData)
		if err != nil {
			return fmt.Errorf("failed to marshal probe data: %w", err)
		}

		req, err := http.NewRequest("POST", m.apiURL+"/probes", strings.NewReader(string(jsonData)))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")

		httpClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to create probe: %w", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("failed to create probe, status: %d", resp.StatusCode)
		}
		// 409 Conflict is OK - probe already exists
	}

	return nil
}

// GetProbeCount returns the number of probes in the API
func (m *RealAPIManager) GetProbeCount() (int, error) {
	apiClient := api.NewClient(m.apiURL+"/probes", "")
	probes, err := apiClient.GetProbes("")
	if err != nil {
		return 0, err
	}
	return len(probes), nil
}

// GetProbe retrieves a specific probe by ID
func (m *RealAPIManager) GetProbe(id string) (*api.Probe, error) {
	resp, err := http.Get(m.apiURL + "/probes/" + id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var probe api.Probe
	if err := json.NewDecoder(resp.Body).Decode(&probe); err != nil {
		return nil, err
	}

	return &probe, nil
}

// ClearAllProbes removes all probes from the API for clean testing
func (m *RealAPIManager) ClearAllProbes() error {
	// Get all probes
	resp, err := http.Get(m.apiURL + "/probes")
	if err != nil {
		return fmt.Errorf("failed to get probes for cleanup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get probes for cleanup, status: %d", resp.StatusCode)
	}

	var probesResponse struct {
		Probes []struct {
			ID string `json:"id"`
		} `json:"probes"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&probesResponse); err != nil {
		return fmt.Errorf("failed to decode probes response: %w", err)
	}

	// Delete each probe
	client := &http.Client{Timeout: 5 * time.Second}
	for _, probe := range probesResponse.Probes {
		req, err := http.NewRequest("DELETE", m.apiURL+"/probes/"+probe.ID, nil)
		if err != nil {
			continue // Skip if we can't create the request
		}

		resp, err := client.Do(req)
		if err != nil {
			continue // Skip if delete fails
		}
		_ = resp.Body.Close()
	}

	return nil
}
