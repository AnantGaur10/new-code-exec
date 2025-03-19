package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"
)

// ExecutionRequest is the initial structure for code execution
type ExecutionRequest struct {
	Code     string `json:"code"`
	Language string `json:"language"`
}

// ExecutionResponse provides output from the execution
type ExecutionResponse struct {
	Output     string        `json:"output"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
	SessionID  string        `json:"sessionId,omitempty"`
	WaitingFor string        `json:"waitingFor,omitempty"` // Indicates if waiting for input
}

// InputRequest contains user input for a running container
type InputRequest struct {
	SessionID string `json:"sessionId"`
	Input     string `json:"input"`
}

// ContainerOutput represents output from a container at a point in time
type ContainerOutput struct {
	Stdout   string
	Stderr   string
	Complete bool
	Error    error
}

// InputResponse represents the response to sending input to a container
type InputResponse struct {
	Output   ContainerOutput
	Error    error
	Complete bool
}

// Container session tracker with channel communication
type containerSession struct {
	containerID     string
	stdin           io.WriteCloser
	hijackedResp    types.HijackedResponse
	outputBuffer    strings.Builder
	errorBuffer     strings.Builder
	lastActive      time.Time
	waiting         bool
	mu              sync.Mutex
	outputChannel   chan ContainerOutput          // Channel for continuous output updates
	inputChannel    chan string                   // Channel for sending input
	responseWaiters map[string]chan InputResponse // Map of channels waiting for responses to inputs
	waitersMutex    sync.Mutex
	done            chan struct{} // Signal when the session is terminated
	ctx             context.Context
	cancel          context.CancelFunc // To cancel all ongoing operations
}

// Global session storage
var (
	sessions      = make(map[string]*containerSession)
	sessionsMutex sync.RWMutex
	cleanupTicker *time.Ticker
)

// enableCORS middleware allows cross-origin requests
func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow requests from localhost:3000
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// executeHandler handles requests to execute code in a container
func executeHandler(w http.ResponseWriter, r *http.Request) {
	// Validate input
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Sanitize input
	if len(req.Code) > 10000 {
		http.Error(w, "Code too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Get language config
	config, ok := languageConfig(req.Language)
	if !ok {
		http.Error(w, "Unsupported language", http.StatusBadRequest)
		return
	}

	// Create session ID
	sessionID := uuid.New().String()

	// Execute in container with channel-based communication
	fmt.Printf("Executing %s code: %s\n", req.Language, req.Code)
	output, waitingForInput, err, duration := startInteractiveContainer(r.Context(), req.Code, config, sessionID)

	if err != nil {
		log.Printf("Execution error: %v", err)
		resp := ExecutionResponse{
			Error: err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp := ExecutionResponse{
		Output:     output,
		Duration:   duration,
		SessionID:  sessionID,
		WaitingFor: waitingForInput,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// inputHandler processes user input for a running container
func inputHandler(w http.ResponseWriter, r *http.Request) {
	// Validate method
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	log.Printf("Received input %v \n", req)

	// Find the session
	sessionsMutex.RLock()
	session, exists := sessions[req.SessionID]
	sessionsMutex.RUnlock()

	if !exists {
		http.Error(w, "Session not found or expired", http.StatusNotFound)
		return
	}
	fmt.Println("Session found for input", req.Input)
	// Create a unique ID for this input request
	inputID := uuid.New().String()
	responseChan := make(chan InputResponse, 1)

	// Register this waiter
	session.waitersMutex.Lock()
	session.responseWaiters[inputID] = responseChan
	session.waitersMutex.Unlock()

	// Update session state
	session.mu.Lock()
	session.lastActive = time.Now()
	session.waiting = false
	session.mu.Unlock()

	// Send input to the container via channel
	select {
	case session.inputChannel <- req.Input + "\n":
		fmt.Println("Input transferred to session.inputChannel")
		// Input sent successfully
	case <-time.After(2 * time.Second):
		// Timeout sending input
		session.waitersMutex.Lock()
		delete(session.responseWaiters, inputID)
		session.waitersMutex.Unlock()
		http.Error(w, "Timeout sending input to container", http.StatusGatewayTimeout)
		return
	case <-session.done:
		// Session terminated
		fmt.Println("Session terminated due to session.done")
		http.Error(w, "Session terminated", http.StatusGone)
		return
	}

	// Wait for response with timeout
	var response InputResponse
	select {
	case response = <-responseChan:
		// Got response
	case <-time.After(5 * time.Second):
		// Timeout waiting for response
		session.waitersMutex.Lock()
		delete(session.responseWaiters, inputID)
		session.waitersMutex.Unlock()

		// Even on timeout, try to get whatever output is available
		session.mu.Lock()
		output := session.outputBuffer.String()
		errorOut := session.errorBuffer.String()
		session.outputBuffer.Reset()
		session.errorBuffer.Reset()
		waitingState := determineIfWaitingForInput(output, errorOut)
		if waitingState != "" {
			session.waiting = true
		}
		session.mu.Unlock()

		// Combine output
		if errorOut != "" {
			if output != "" {
				output += "\n\nErrors:\n" + errorOut
			} else {
				output = errorOut
			}
		}

		resp := ExecutionResponse{
			Output:     output,
			SessionID:  req.SessionID,
			WaitingFor: waitingState,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	case <-session.done:
		// Session terminated while waiting
		http.Error(w, "Session terminated while processing input", http.StatusGone)
		return
	}

	// Clean up the waiter
	session.waitersMutex.Lock()
	delete(session.responseWaiters, inputID)
	session.waitersMutex.Unlock()

	// Check for error
	if response.Error != nil {
		http.Error(w, response.Error.Error(), http.StatusInternalServerError)
		return
	}

	// Combine output
	output := response.Output.Stdout
	errorOut := response.Output.Stderr

	if errorOut != "" {
		if output != "" {
			output += "\n\nErrors:\n" + errorOut
		} else {
			output = errorOut
		}
	}

	// Check if waiting for more input
	waitingState := determineIfWaitingForInput(output, errorOut)
	fmt.Println("Waiting state is ", waitingState)
	resp := ExecutionResponse{
		Output:     output,
		SessionID:  req.SessionID,
		WaitingFor: waitingState,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// determineIfWaitingForInput checks if the program seems to be waiting for user input
func determineIfWaitingForInput(stdout, stderr string) string {
	fmt.Println("Determining is crazy")
	// Common patterns that indicate waiting for input
	waitingPatterns := []string{
		"input", "enter", "?: ", "> ", "prompt:",
		"scanf", "cin", "readline", "read", "gets",
	}

	// Check both stdout and stderr for waiting patterns
	combined := stdout + stderr
	lowerCombined := strings.ToLower(combined)

	// Check if the last part of output looks like it's waiting for input
	for _, pattern := range waitingPatterns {
		if strings.Contains(lowerCombined, pattern) &&
			!strings.HasSuffix(combined, "\n") {
			return "input"
		}
	}

	return ""
}

// languageConfig returns the container configuration for a given language
func languageConfig(lang string) (container.Config, bool) {
	configs := map[string]container.Config{
		"python": {
			Image:        "python:3.9-slim",
			Cmd:          []string{"python", "-c"},
			Tty:          true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    false,
		},
		"javascript": {
			Image:        "node:18-slim",
			Cmd:          []string{"node", "-e"},
			Tty:          true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    false,
		},
		"go": {
			Image:        "golang:1.23.7-alpine",
			Cmd:          []string{"sh", "-c", "mkdir -p /app && echo \"$CODE\" > /app/main.go && cd /app && go run main.go"},
			Env:          []string{"CODE=${CODE}"},
			Tty:          true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    false,
		},
		"c": {
			Image:        "gcc:latest",
			Cmd:          []string{"sh", "-c", "echo \"$CODE\" > /tmp/program.c && gcc /tmp/program.c -o /tmp/program && /tmp/program"},
			Env:          []string{"CODE=${CODE}"},
			Tty:          true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    false,
		},
	}
	cfg, exists := configs[lang]
	return cfg, exists
}

// startInteractiveContainer creates and starts a Docker container for code execution
func startInteractiveContainer(ctx context.Context, code string, config container.Config, sessionID string) (string, string, error, time.Duration) {
	startTime := time.Now()

	// Create a context with timeout for the container operations
	containerCtx, cancelContainer := context.WithTimeout(ctx, 30*time.Second)

	// Initialize Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		cancelContainer()
		return "", "", fmt.Errorf("docker init failed: %w", err), time.Since(startTime)
	}
	// Create channels for container output
	outputCh := make(chan ContainerOutput, 10)
	inputCh := make(chan string, 10)
	doneCh := make(chan struct{})

	// Generate unique ID for this execution
	execID := "exec-" + sessionID

	// Configure container for the language
	var hostConfig container.HostConfig

	// Handle Go code differently than other languages
	if strings.HasPrefix(config.Image, "golang") {
		// Create a temporary directory to store the code file
		tempDir := "/tmp/" + execID
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			cancelContainer()
			return "", "", fmt.Errorf("failed to create temp directory: %w", err), time.Since(startTime)
		}

		// Write the code directly to a file
		if err := os.WriteFile(tempDir+"/main.go", []byte(code), 0644); err != nil {
			cancelContainer()
			return "", "", fmt.Errorf("failed to write code file: %w", err), time.Since(startTime)
		}

		// Update config to use the mounted file
		config.Cmd = []string{"go", "run", "/code/main.go"}
		config.WorkingDir = "/code"

		// Configure host with bind mount and resource limits
		hostConfig = container.HostConfig{
			Binds: []string{tempDir + ":/code:ro"},
			Resources: container.Resources{
				Memory:     256 * 1024 * 1024, // 256MB
				MemorySwap: 256 * 1024 * 1024, // Disable swap
				NanoCPUs:   2e9,               // 2 CPU cores
			},
		}
	} else if strings.HasPrefix(config.Image, "gcc") {
		// For C code, store it in an environment variable
		config.Env = []string{fmt.Sprintf("CODE=%s", code)}

		// Standard resource limits
		hostConfig = container.HostConfig{
			Resources: container.Resources{
				Memory:     256 * 1024 * 1024,
				MemorySwap: 256 * 1024 * 1024,
				NanoCPUs:   2e9,
			},
		}
	} else {
		// For other languages, append the code to the command
		config.Cmd = append(config.Cmd, code)

		// Standard resource limits
		hostConfig = container.HostConfig{
			Resources: container.Resources{
				Memory:     256 * 1024 * 1024,
				MemorySwap: 256 * 1024 * 1024,
				NanoCPUs:   2e9,
			},
		}
	}

	// Create container
	resp, err := cli.ContainerCreate(containerCtx, &config, &hostConfig, nil, nil, execID)
	if err != nil {
		cancelContainer()
		return "", "", fmt.Errorf("container create failed: %w", err), time.Since(startTime)
	}
	log.Printf("Container creation completed %v \n", resp)

	// Attach to container for I/O
	hijackedResp, err := cli.ContainerAttach(containerCtx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		log.Println("Cleaning up container because", err)
		cancelContainer()
		// Clean up the container
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer removeCancel()
		cli.ContainerRemove(removeCtx, resp.ID, container.RemoveOptions{Force: true})
		return "", "", fmt.Errorf("container attach failed: %w", err), time.Since(startTime)
	}
	fmt.Printf("Hijacked response is %v \n", hijackedResp)
	// Create session to track this container
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	session := &containerSession{
		containerID:     resp.ID,
		stdin:           hijackedResp.Conn,
		hijackedResp:    hijackedResp,
		lastActive:      time.Now(),
		outputChannel:   outputCh,
		inputChannel:    inputCh,
		responseWaiters: make(map[string]chan InputResponse),
		done:            doneCh,
		ctx:             sessionCtx,
		cancel:          sessionCancel,
	}

	// Store the session
	sessionsMutex.Lock()
	sessions[sessionID] = session
	sessionsMutex.Unlock()

	// Start container
	if err := cli.ContainerStart(containerCtx, resp.ID, container.StartOptions{}); err != nil {
		hijackedResp.Close()
		sessionCancel()
		close(doneCh)

		// Remove the session
		sessionsMutex.Lock()
		delete(sessions, sessionID)
		sessionsMutex.Unlock()

		// Clean up the container
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer removeCancel()
		cli.ContainerRemove(removeCtx, resp.ID, container.RemoveOptions{Force: true})
		fmt.Println("Cleaning up beacuse container didnt start")
		return "", "", fmt.Errorf("container start failed: %w", err), time.Since(startTime)
	}
	log.Printf("Container Started \n")
	// Start goroutines for I/O handling
	go handleContainerIO(session, sessionID)

	// Wait for initial output with timeout
	var initialOutput ContainerOutput
	outputTimeout := time.After(15 * time.Second)
	outputReceived := false

	fmt.Println("Waiting for container output...")

	// Instead of just waiting once, try to collect output until timeout
	for !outputReceived {
		select {
		case output := <-outputCh:
			fmt.Printf("Received output: stdout=[%s], stderr=[%s]\n", output.Stdout, output.Stderr)

			// Append to our initial output
			initialOutput.Stdout += output.Stdout
			initialOutput.Stderr += output.Stderr

			// Check if we have meaningful output to return
			if len(initialOutput.Stdout) > 0 || len(initialOutput.Stderr) > 0 {
				outputReceived = true
			}

		case <-outputTimeout:
			// Timed out waiting for output
			fmt.Println("Timed out waiting for container output")

			// Get whatever might be in the buffer
			session.mu.Lock()
			initialOutput = ContainerOutput{
				Stdout: session.outputBuffer.String(),
				Stderr: session.errorBuffer.String(),
			}
			session.mu.Unlock()

			outputReceived = true
		}
	}

	// Check if waiting for input
	waitingState := determineIfWaitingForInput(initialOutput.Stdout, initialOutput.Stderr)
	if waitingState != "" {
		session.mu.Lock()
		session.waiting = true
		session.mu.Unlock()
	}

	// Combine output
	output := initialOutput.Stdout
	fmt.Println("output is ", output)
	if initialOutput.Stderr != "" {
		if output != "" {
			output += "\n\nErrors:\n" + initialOutput.Stderr
		} else {
			output = initialOutput.Stderr
		}
	}

	// Clean up the container context - but keep the session running
	cancelContainer()

	duration := time.Since(startTime)
	fmt.Println("Duration took is ", duration)
	return output, waitingState, nil, duration
}

// handleContainerIO manages all I/O for a container session
func handleContainerIO(session *containerSession, sessionID string) {
	// Create readers for stdout and stderr
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	// Start demuxing in a goroutine
	go func() {
		defer stdoutWriter.Close()
		defer stderrWriter.Close()
		stdcopy.StdCopy(stdoutWriter, stderrWriter, session.hijackedResp.Reader)
		fmt.Println("Completed demultiplexing")
	}()

	// Output processing goroutines
	var wg sync.WaitGroup
	wg.Add(2)

	// Process stdout
	go func() {
		defer wg.Done()
		processStreamToBuffer(stdoutReader, &session.outputBuffer, false, session)
	}()

	// Process stderr
	go func() {
		defer wg.Done()
		processStreamToBuffer(stderrReader, &session.errorBuffer, true, session)
	}()
	go func() {
		// Wait for stdout and stderr processing to complete
		wg.Wait()
		log.Printf("Stream processing complete for session: %s", sessionID)
	}()

	// Input forwarding goroutine
	go func() {
		for {
			select {
			case input := <-session.inputChannel:
				// Send input to container
				_, err := session.stdin.Write([]byte(input))
				if err != nil {
					log.Printf("Error writing to container stdin: %v", err)
					return
				}

				// Wait briefly for the program to process input
				time.Sleep(100 * time.Millisecond)

				// Get current output
				session.mu.Lock()
				output := session.outputBuffer.String()
				errorOut := session.errorBuffer.String()

				// Clear buffers
				session.outputBuffer.Reset()
				session.errorBuffer.Reset()
				session.mu.Unlock()

				// Notify all waiting input responders
				session.waitersMutex.Lock()
				for _, ch := range session.responseWaiters {
					ch <- InputResponse{
						Output: ContainerOutput{
							Stdout: output,
							Stderr: errorOut,
						},
						Error: nil,
					}
				}
				session.waitersMutex.Unlock()

			case <-session.ctx.Done():
				return
			}
		}
	}()

	// Collect output periodically and send to the output channel
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			session.mu.Lock()
			stdout := session.outputBuffer.String()
			stderr := session.errorBuffer.String()

			// Only send if there's actual output
			if stdout != "" || stderr != "" {
				session.outputChannel <- ContainerOutput{
					Stdout: stdout,
					Stderr: stderr,
				}

				// Clear the buffers
				session.outputBuffer.Reset()
				session.errorBuffer.Reset()
			}
			session.mu.Unlock()

		case <-session.ctx.Done():
			// Session was canceled, clean up
			session.stdin.Close()
			session.hijackedResp.Close()
			close(session.done)

			// Remove the container
			go func(containerID string) {
				cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
				if err != nil {
					log.Printf("Error connecting to Docker for cleanup: %v", err)
					return
				}
				defer cli.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				fmt.Println("Removing the container inside handleContainer")
				// Try to stop and remove the container
				cli.ContainerStop(ctx, containerID, container.StopOptions{})
				cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			}(session.containerID)

			return
		}
	}
}

// processStreamToBuffer reads from a stream and writes to a buffer
func processStreamToBuffer(reader io.Reader, buffer *strings.Builder, isError bool, session *containerSession) {
	streamType := "stdout"
	if isError {
		streamType = "stderr"
	}
	fmt.Printf("Starting to process %s stream\n", streamType)

	buf := make([]byte, 1024)
	for {
		select {
		case <-session.ctx.Done():
			fmt.Printf("%s stream processing canceled\n", streamType)
			return
		default:
			n, err := reader.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from %s: %v", streamType, err)
				} else {
					fmt.Printf("EOF reached for %s\n", streamType)
				}
				return
			}

			if n > 0 {
				data := string(buf[:n])
				fmt.Printf("Read %d bytes from %s: %s\n", n, streamType, data)

				session.mu.Lock()
				buffer.Write(buf[:n])
				session.lastActive = time.Now()
				session.mu.Unlock()

				// Instead of waiting for the ticker, send data immediately
				containerOutput := ContainerOutput{
					Stdout: "",
					Stderr: "",
				}

				if isError {
					containerOutput.Stderr = data
				} else {
					containerOutput.Stdout = data
				}

				// Try to send to channel without blocking
				select {
				case session.outputChannel <- containerOutput:
					fmt.Printf("Sent %s data to output channel\n", streamType)
				default:
					fmt.Printf("Output channel full, couldn't send %s data\n", streamType)
				}
			}
		}
	}
}

// statusHandler returns the current status of a session
func statusHandler(w http.ResponseWriter, r *http.Request) {
	// Extract session ID from query parameters
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, "SessionID is required", http.StatusBadRequest)
		return
	}

	// Find the session
	sessionsMutex.RLock()
	session, exists := sessions[sessionID]
	sessionsMutex.RUnlock()

	if !exists {
		http.Error(w, "Session not found or expired", http.StatusNotFound)
		return
	}

	// Update last active time
	session.mu.Lock()
	session.lastActive = time.Now()

	// Get current output
	output := session.outputBuffer.String()
	errorOut := session.errorBuffer.String()

	// Check if waiting for more input
	waitingState := determineIfWaitingForInput(output, errorOut)
	if waitingState != "" {
		session.waiting = true
	}

	// Clear buffers for next read
	session.outputBuffer.Reset()
	session.errorBuffer.Reset()
	session.mu.Unlock()

	// Combine output
	if errorOut != "" {
		if output != "" {
			output += "\n\nErrors:\n" + errorOut
		} else {
			output = errorOut
		}
	}

	resp := ExecutionResponse{
		Output:     output,
		SessionID:  sessionID,
		WaitingFor: waitingState,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// cleanupInactiveSessions removes sessions that have been inactive too long
func cleanupInactiveSessions() {
	fmt.Println("Checking for clean up")
	sessionsMutex.Lock()
	defer sessionsMutex.Unlock()

	now := time.Now()
	for id, session := range sessions {
		// If inactive for more than 5 minutes or not waiting for input for more than 2 minutes
		if (now.Sub(session.lastActive) > 5*time.Minute) ||
			(!session.waiting && now.Sub(session.lastActive) > 2*time.Minute) {

			// Cancel the session context and trigger cleanup
			session.cancel()

			// Remove from sessions map
			delete(sessions, id)
			log.Printf("Cleaned up inactive session: %s", id)
		}
	}
}

// Rate limiter middleware with memory cleanup
func rateLimit(next http.HandlerFunc) http.HandlerFunc {
	var (
		mu          sync.Mutex
		counts      = make(map[string]int)
		lastCleaned = time.Now()
	)

	return func(w http.ResponseWriter, r *http.Request) {
		// Extract client IP properly
		ip := getClientIP(r)

		mu.Lock()

		// Clean up old entries periodically
		if time.Since(lastCleaned) > 10*time.Minute {
			counts = make(map[string]int)
			lastCleaned = time.Now()
		}

		counts[ip]++
		if counts[ip] > 20 { // Increased limit for interactive sessions
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "Rate limit exceeded. Maximum 20 requests per minute allowed.",
			})
			return
		}
		mu.Unlock()

		time.AfterFunc(time.Minute, func() {
			mu.Lock()
			counts[ip]--
			if counts[ip] <= 0 {
				delete(counts, ip) // Clean up
			}
			mu.Unlock()
		})

		next(w, r)
	}
}

// Get client IP, handling X-Forwarded-For headers
func getClientIP(r *http.Request) string {
	// Check for X-Forwarded-For header first (for proxy setups)
	forwardedIP := r.Header.Get("X-Forwarded-For")
	if forwardedIP != "" {
		// Use the first IP in the list (client IP)
		ips := strings.Split(forwardedIP, ",")
		return strings.TrimSpace(ips[0])
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// If SplitHostPort fails, return the whole RemoteAddr
		return r.RemoteAddr
	}
	return ip
}

func main() {
	// Make sure Docker client can connect
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Cannot connect to Docker daemon: %v", err)
	}
	cli.Close()

	var port string = "8000"

	// Start session cleanup ticker
	cleanupTicker = time.NewTicker(1 * time.Minute)
	go func() {
		for range cleanupTicker.C {
			cleanupInactiveSessions()
		}
	}()

	log.Printf("Starting interactive code execution server on port %s", port)
	mux := http.NewServeMux()
	handler := enableCORS(mux)

	// Add routes
	mux.HandleFunc("/execute", rateLimit(executeHandler))
	mux.HandleFunc("/input", rateLimit(inputHandler))
	mux.HandleFunc("/status", rateLimit(statusHandler))

	log.Fatal(http.ListenAndServe(":"+port, handler))
}
