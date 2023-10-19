package llm

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmorganca/ollama/api"
	"github.com/jmorganca/ollama/format"
)

//go:embed llama.cpp/*/build/*/bin/*
var llamaCppEmbed embed.FS

type ModelRunner struct {
	Path        string // path to the model runner executable
	Accelerated bool
}

func chooseRunners(workDir, runnerType string) []ModelRunner {
	buildPath := path.Join("llama.cpp", runnerType, "build")
	var runners []ModelRunner

	// set the runners based on the OS
	// IMPORTANT: the order of the runners in the array is the priority order
	switch runtime.GOOS {
	case "darwin":
		runners = []ModelRunner{
			{Path: path.Join(buildPath, "metal", "bin", "ollama-runner")},
			{Path: path.Join(buildPath, "cpu", "bin", "ollama-runner")},
		}
	case "linux":
		runners = []ModelRunner{
			{Path: path.Join(buildPath, "cuda", "bin", "ollama-runner"), Accelerated: true},
			{Path: path.Join(buildPath, "cpu", "bin", "ollama-runner")},
		}
	case "windows":
		// TODO: select windows GPU runner here when available
		runners = []ModelRunner{
			{Path: path.Join(buildPath, "cpu", "bin", "Release", "ollama-runner.exe")},
		}
	default:
		log.Printf("unknown OS, running on CPU: %s", runtime.GOOS)
		runners = []ModelRunner{
			{Path: path.Join(buildPath, "cpu", "bin", "ollama-runner")},
		}
	}

	runnerAvailable := false // if no runner files are found in the embed, this flag will cause a fast fail
	for _, r := range runners {
		// find all the files in the runner's bin directory
		files, err := fs.Glob(llamaCppEmbed, path.Join(path.Dir(r.Path), "*"))
		if err != nil {
			// this is expected, ollama may be compiled without all runners packed in
			log.Printf("%s runner not found: %v", r.Path, err)
			continue
		}

		for _, f := range files {
			runnerAvailable = true

			srcFile, err := llamaCppEmbed.Open(f)
			if err != nil {
				log.Fatalf("read llama runner %s: %v", f, err)
			}
			defer srcFile.Close()

			// create the directory in case it does not exist, filepath.Dir() converts the file path to the OS's format
			destPath := filepath.Join(workDir, filepath.Dir(f))
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				log.Fatalf("create runner temp dir %s: %v", filepath.Dir(f), err)
			}

			// create the path to the destination file, filepath.Base() converts the file path to the OS's format
			destFile := filepath.Join(destPath, filepath.Base(f))

			_, err = os.Stat(destFile)
			switch {
			case errors.Is(err, os.ErrNotExist):
				destFile, err := os.OpenFile(destFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
				if err != nil {
					log.Fatalf("write llama runner %s: %v", f, err)
				}
				defer destFile.Close()

				if _, err := io.Copy(destFile, srcFile); err != nil {
					log.Fatalf("copy llama runner %s: %v", f, err)
				}
			case err != nil:
				log.Fatalf("stat llama runner %s: %v", f, err)
			}
		}
	}
	if !runnerAvailable {
		log.Fatalf("%s runner not found", runnerType)
	}

	// return the runners to try in priority order
	localRunnersByPriority := []ModelRunner{}
	for _, r := range runners {
		// clean the ModelRunner paths so that they match the OS we are running on
		localRunnersByPriority = append(localRunnersByPriority, ModelRunner{
			Path:        filepath.Clean(path.Join(workDir, r.Path)),
			Accelerated: r.Accelerated,
		})
	}

	return localRunnersByPriority
}

type llamaModel struct {
	hyperparameters llamaHyperparameters
}

func (llm *llamaModel) ModelFamily() string {
	return "llama"
}

func llamaModelType(numLayer uint32) string {
	switch numLayer {
	case 26:
		return "3B"
	case 32:
		return "7B"
	case 40:
		return "13B"
	case 48:
		return "34B"
	case 60:
		return "30B"
	case 80:
		return "65B"
	default:
		return "unknown"
	}
}

func (llm *llamaModel) ModelType() string {
	return llamaModelType(llm.hyperparameters.NumLayer)
}

func (llm *llamaModel) FileType() string {
	return fileType(llm.hyperparameters.FileType)
}

func (llm *llamaModel) NumLayers() int64 {
	return int64(llm.hyperparameters.NumLayer)
}

type llamaHyperparameters struct {
	// NumVocab is the size of the model's vocabulary.
	NumVocab uint32

	// NumEmbd is the size of the model's embedding layer.
	NumEmbd uint32
	NumMult uint32
	NumHead uint32

	// NumLayer is the number of layers in the model.
	NumLayer uint32
	NumRot   uint32

	// FileType describes the quantization level of the model, e.g. Q4_0, Q5_K, etc.
	FileType uint32
}

type Running struct {
	Port          int
	Cmd           *exec.Cmd
	Cancel        context.CancelFunc
	exitOnce      sync.Once
	exitCh        chan error // channel to receive the exit status of the subprocess
	*StatusWriter            // captures error messages from the llama runner process
}

type llama struct {
	api.Options
	Running
}

// StatusWriter is a writer that captures error messages from the llama runner process
type StatusWriter struct {
	ErrCh      chan error
	LastErrMsg string
}

func NewStatusWriter() *StatusWriter {
	return &StatusWriter{
		ErrCh: make(chan error, 1),
	}
}

func (w *StatusWriter) Write(b []byte) (int, error) {
	var errMsg string
	if _, after, ok := bytes.Cut(b, []byte("error:")); ok {
		errMsg = string(bytes.TrimSpace(after))
	} else if _, after, ok := bytes.Cut(b, []byte("CUDA error")); ok {
		errMsg = string(bytes.TrimSpace(after))
	}

	if errMsg != "" {
		w.LastErrMsg = errMsg
		w.ErrCh <- fmt.Errorf("llama runner: %s", errMsg)
	}

	return os.Stderr.Write(b)
}

func newLlama(model string, adapters []string, runners []ModelRunner, numLayers int64, opts api.Options) (*llama, error) {
	if len(adapters) > 1 {
		return nil, errors.New("ollama supports only one lora adapter, but multiple were provided")
	}

	if opts.NumGPU == -1 && runtime.GOOS == "darwin" {
		// default to enable metal on macOS
		opts.NumGPU = 1
	}

	params := []string{
		"--model", model,
		"--ctx-size", fmt.Sprintf("%d", opts.NumCtx),
		"--rope-freq-base", fmt.Sprintf("%f", opts.RopeFrequencyBase),
		"--rope-freq-scale", fmt.Sprintf("%f", opts.RopeFrequencyScale),
		"--batch-size", fmt.Sprintf("%d", opts.NumBatch),
		"--n-gpu-layers", fmt.Sprintf("%d", opts.NumGPU),
		"--embedding",
	}

	if opts.NumGQA > 0 {
		params = append(params, "--gqa", fmt.Sprintf("%d", opts.NumGQA))
	}

	if len(adapters) > 0 {
		// TODO: applying multiple adapters is not supported by the llama.cpp server yet
		params = append(params, "--lora", adapters[0])
	}

	if opts.NumThread > 0 {
		params = append(params, "--threads", fmt.Sprintf("%d", opts.NumThread))
	}

	if !opts.F16KV {
		params = append(params, "--memory-f32")
	}
	if opts.UseMLock {
		params = append(params, "--mlock")
	}
	if !opts.UseMMap {
		params = append(params, "--no-mmap")
	}
	if opts.UseNUMA {
		params = append(params, "--numa")
	}

	var runnerErr error

	// start the llama.cpp server with a retry in case the port is already in use
	for _, runner := range runners {
		if runner.Accelerated && opts.NumGPU == 0 {
			log.Printf("skipping accelerated runner because num_gpu=0")
			continue
		}

		if _, err := os.Stat(runner.Path); err != nil {
			log.Printf("llama runner not found: %v", err)
			continue
		}

		port := rand.Intn(65535-49152) + 49152 // get a random port in the ephemeral range
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(
			ctx,
			runner.Path,
			append(params, "--port", strconv.Itoa(port))...,
		)
		cmd.Env = append(os.Environ(), fmt.Sprintf("LD_LIBRARY_PATH=%s", filepath.Dir(runner.Path)))
		cmd.Stdout = os.Stderr
		statusWriter := NewStatusWriter()
		cmd.Stderr = statusWriter

		llm := &llama{Options: opts, Running: Running{Port: port, Cmd: cmd, Cancel: cancel, exitCh: make(chan error)}}

		log.Print("starting llama runner")
		if err := llm.Cmd.Start(); err != nil {
			log.Printf("error starting the external llama runner: %v", err)
			continue
		}

		// monitor the llama runner process and signal when it exits
		go func() {
			err := llm.Cmd.Wait()
			// default to printing the exit message of the command process, it will probably just say 'exit staus 1'
			errMsg := err.Error()
			// try to set a better error message if llama runner logs captured an error
			if statusWriter.LastErrMsg != "" {
				errMsg = statusWriter.LastErrMsg
			}
			log.Println(errMsg)
			// llm.Cmd.Wait() can only be called once, use this exit channel to signal that the process has exited
			llm.exitOnce.Do(func() {
				close(llm.exitCh)
			})
		}()

		if err := waitForServer(llm); err != nil {
			log.Printf("error starting llama runner: %v", err)
			llm.Close()

			// default the runnerErr to the error returned by the most recent llama runner process
			runnerErr = err

			// capture the error directly from the runner process, if any
			select {
			case runnerErr = <-statusWriter.ErrCh:
			default:
				// the runner process probably timed out
			}

			// try again
			continue
		}

		// server started successfully
		return llm, nil
	}

	if runnerErr != nil {
		// this is the error returned from the llama runner process that failed most recently
		return nil, runnerErr
	}

	return nil, fmt.Errorf("failed to start a llama runner")
}

func waitForServer(llm *llama) error {
	start := time.Now()
	expiresAt := time.Now().Add(3 * time.Minute) // be generous with timeout, large models can take a while to load
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	log.Print("waiting for llama runner to start responding")
	for {
		select {
		case <-llm.exitCh:
			// failed to start subprocess
			return fmt.Errorf("llama runner process has terminated")
		case <-ticker.C:
			if time.Now().After(expiresAt) {
				// timeout
				return fmt.Errorf("timed out waiting for llama runner to start")
			}

			if err := llm.Ping(context.Background()); err == nil {
				// success
				log.Printf("llama runner started in %f seconds", time.Since(start).Seconds())
				return nil
			}
		}
	}
}

func (llm *llama) Close() {
	// signal the sub-process to terminate
	llm.Cancel()

	// wait for the command to exit to prevent race conditions with the next run
	<-llm.exitCh

	if llm.StatusWriter != nil && llm.StatusWriter.LastErrMsg != "" {
		log.Printf("llama runner stopped with error: %v", llm.StatusWriter.LastErrMsg)
	} else {
		log.Print("llama runner stopped successfully")
	}
}

func (llm *llama) SetOptions(opts api.Options) {
	llm.Options = opts
}

type prediction struct {
	Content string `json:"content"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Stop    bool   `json:"stop"`

	Timings struct {
		PredictedN  int     `json:"predicted_n"`
		PredictedMS float64 `json:"predicted_ms"`
		PromptN     int     `json:"prompt_n"`
		PromptMS    float64 `json:"prompt_ms"`
	}
}

const maxBufferSize = 512 * format.KiloByte

func (llm *llama) Predict(ctx context.Context, prevContext []int, prompt string, fn func(api.GenerateResponse)) error {
	prevConvo, err := llm.Decode(ctx, prevContext)
	if err != nil {
		return err
	}

	// Remove leading spaces from prevConvo if present
	prevConvo = strings.TrimPrefix(prevConvo, " ")

	var nextContext strings.Builder
	nextContext.WriteString(prevConvo)
	nextContext.WriteString(prompt)

	request := map[string]any{
		"prompt":            nextContext.String(),
		"stream":            true,
		"n_predict":         llm.NumPredict,
		"n_keep":            llm.NumKeep,
		"temperature":       llm.Temperature,
		"top_k":             llm.TopK,
		"top_p":             llm.TopP,
		"tfs_z":             llm.TFSZ,
		"typical_p":         llm.TypicalP,
		"repeat_last_n":     llm.RepeatLastN,
		"repeat_penalty":    llm.RepeatPenalty,
		"presence_penalty":  llm.PresencePenalty,
		"frequency_penalty": llm.FrequencyPenalty,
		"mirostat":          llm.Mirostat,
		"mirostat_tau":      llm.MirostatTau,
		"mirostat_eta":      llm.MirostatEta,
		"penalize_nl":       llm.PenalizeNewline,
		"seed":              llm.Seed,
		"stop":              llm.Stop,
	}

	// Handling JSON marshaling with special characters unescaped.
	buffer := &bytes.Buffer{}
	enc := json.NewEncoder(buffer)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(request); err != nil {
		return fmt.Errorf("failed to marshal data: %v", err)
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/completion", llm.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, buffer)
	if err != nil {
		return fmt.Errorf("error creating POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST predict: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed reading llm error response: %w", err)
		}
		log.Printf("llm predict error: %s", bodyBytes)
		return fmt.Errorf("%s", bodyBytes)
	}

	scanner := bufio.NewScanner(resp.Body)
	// increase the buffer size to avoid running out of space
	buf := make([]byte, 0, maxBufferSize)
	scanner.Buffer(buf, maxBufferSize)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			// This handles the request cancellation
			return ctx.Err()
		default:
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			if evt, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				var p prediction
				if err := json.Unmarshal(evt, &p); err != nil {
					return fmt.Errorf("error unmarshaling llm prediction response: %v", err)
				}

				if p.Content != "" {
					fn(api.GenerateResponse{Response: p.Content})
					nextContext.WriteString(p.Content)
				}

				if p.Stop {
					embd, err := llm.Encode(ctx, nextContext.String())
					if err != nil {
						return fmt.Errorf("encoding context: %v", err)
					}

					fn(api.GenerateResponse{
						Done:               true,
						Context:            embd,
						PromptEvalCount:    p.Timings.PromptN,
						PromptEvalDuration: parseDurationMs(p.Timings.PromptMS),
						EvalCount:          p.Timings.PredictedN,
						EvalDuration:       parseDurationMs(p.Timings.PredictedMS),
					})

					return nil
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "unexpected EOF") {
			// this means the llama runner subprocess crashed
			llm.Close()
			if llm.StatusWriter != nil && llm.StatusWriter.LastErrMsg != "" {
				return fmt.Errorf("llama runner exited: %v", llm.StatusWriter.LastErrMsg)
			}
			return fmt.Errorf("llama runner exited, you may not have enough available memory to run this model")
		}
		return fmt.Errorf("error reading llm response: %v", err)
	}

	return nil
}

type TokenizeRequest struct {
	Content string `json:"content"`
}

type TokenizeResponse struct {
	Tokens []int `json:"tokens"`
}

func (llm *llama) Encode(ctx context.Context, prompt string) ([]int, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/tokenize", llm.Port)
	data, err := json.Marshal(TokenizeRequest{Content: prompt})
	if err != nil {
		return nil, fmt.Errorf("marshaling encode data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do encode request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read encode request: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm encode error: %s", body)
		return nil, fmt.Errorf("%s", body)
	}

	var encoded TokenizeResponse
	if err := json.Unmarshal(body, &encoded); err != nil {
		return nil, fmt.Errorf("unmarshal encode response: %w", err)
	}

	return encoded.Tokens, nil
}

type DetokenizeRequest struct {
	Tokens []int `json:"tokens"`
}

type DetokenizeResponse struct {
	Content string `json:"content"`
}

func (llm *llama) Decode(ctx context.Context, tokens []int) (string, error) {
	if len(tokens) == 0 {
		return "", nil
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/detokenize", llm.Port)
	data, err := json.Marshal(DetokenizeRequest{Tokens: tokens})
	if err != nil {
		return "", fmt.Errorf("marshaling decode data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return "", fmt.Errorf("decode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do decode request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read decode request: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm decode error: %s", body)
		return "", fmt.Errorf("%s", body)
	}

	var decoded DetokenizeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("unmarshal encode response: %w", err)
	}

	return decoded.Content, nil
}

type EmbeddingRequest struct {
	Content string `json:"content"`
}

type EmbeddingResponse struct {
	Embedding []float64 `json:"embedding"`
}

func (llm *llama) Embedding(ctx context.Context, input string) ([]float64, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/embedding", llm.Port)
	data, err := json.Marshal(TokenizeRequest{Content: input})
	if err != nil {
		return nil, fmt.Errorf("error marshaling embed data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("error creating embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST embedding: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading embed response: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm encode error: %s", body)
		return nil, fmt.Errorf("%s", body)
	}

	var embedding EmbeddingResponse
	if err := json.Unmarshal(body, &embedding); err != nil {
		return nil, fmt.Errorf("unmarshal tokenize response: %w", err)
	}

	return embedding.Embedding, nil
}

// Ping checks that the server subprocess is still running and responding to requests
func (llm *llama) Ping(ctx context.Context) error {
	resp, err := http.Head(fmt.Sprintf("http://127.0.0.1:%d", llm.Port))
	if err != nil {
		return fmt.Errorf("ping resp: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected ping status: %s", resp.Status)
	}
	return nil
}
