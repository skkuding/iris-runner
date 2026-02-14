package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// RunnerServer: Box Pool을 관리하는 서버 구조체
type RunnerServer struct {
	boxPool  chan int32 // 사용 가능한 Box ID를 저장하는 채널
	maxBoxes int32      // 최대 Box 개수
}

type Message struct {
	Type     string `json:"type"`     // "code", "input", "echo" 등
	Language string `json:"language"` // c, cpp, java, go, python, ...
	Source   string `json:"source"`   // 소스코드
	Data     string `json:"data"`     // "input" 메시지에서 사용 (stdin에 보낼 내용)
}

// ConnectionContext: 한 WebSocket 커넥션에서 실행 중인 프로세스 정보를 저장
type ConnectionContext struct {
	conn      *websocket.Conn
	cmd       *exec.Cmd
	stdinPipe io.WriteCloser
	boxID     int32 // 할당된 isolate Box ID

	// 표준출력/표준에러를 중복해서 웹소켓에 보내지 않도록 보호하는 뮤텍스 등
	mu sync.Mutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// NewRunnerServer: RunnerServer 인스턴스 생성
func NewRunnerServer(maxBoxes int32) *RunnerServer {
	server := &RunnerServer{
		boxPool:  make(chan int32, maxBoxes),
		maxBoxes: maxBoxes,
	}
	return server
}

// initializeBoxPool: isolate --init을 실행하여 Box Pool 초기화
func (s *RunnerServer) initializeBoxPool() error {
	log.Printf("Initializing %d isolate boxes...\n", s.maxBoxes)

	for i := int32(0); i < s.maxBoxes; i++ {
		cmd := exec.Command("isolate", "--cg", "--box-id", fmt.Sprintf("%d", i), "--init")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to init box %d: %v, output: %s", i, err, output)
		}
		log.Printf("Initialized box %d: %s", i, strings.TrimSpace(string(output)))

		// 초기화된 Box ID를 채널에 추가
		s.boxPool <- i
	}

	log.Println("All boxes initialized successfully")
	return nil
}

// healthHandler: 서버 상태 및 Box Pool 사용률 반환
func (s *RunnerServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	available := len(s.boxPool)
	inUse := int(s.maxBoxes) - available

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "healthy",
		"maxBoxes":  s.maxBoxes,
		"available": available,
		"inUse":     inUse,
	})
}

func main() {
	// Runner Server 생성 (최대 100개 Box)
	server := NewRunnerServer(100)

	// Box Pool 초기화
	if err := server.initializeBoxPool(); err != nil {
		log.Fatalf("Failed to initialize box pool: %v", err)
	}

	// Health check endpoint 추가
	http.HandleFunc("/health", server.healthHandler)
	http.HandleFunc("/ws", server.wsHandler)

	addr := ":8000"
	log.Printf("WebSocket server running on %s with %d boxes\n", addr, server.maxBoxes)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func (s *RunnerServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Upgrade 전에 capacity 체크 (빠른 거절)
	if len(s.boxPool) == 0 {
		log.Println("No available boxes, rejecting connection")
		http.Error(w, "Server is busy. No available boxes. Please try again later.", http.StatusServiceUnavailable)
		return
	}

	// 2. WebSocket Upgrade (capacity 확인 후)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	// 3. Box 할당 (타임아웃 포함)
	// 체크와 할당 사이의 시간 차로 인한 TOCTOU 문제 방어
	// (체크 시 available했지만, Upgrade 중 다른 요청이 가져갈 수 있음)
	var boxID int32
	select {
	case boxID = <-s.boxPool:
		log.Printf("Box %d allocated\n", boxID)
	case <-time.After(5 * time.Second):
		// Upgrade 후 Box를 못 받는 경우 (TOCTOU)
		log.Println("Failed to allocate box within timeout")
		sendJSON(conn, map[string]interface{}{
			"type":  "error",
			"error": "Failed to allocate box. Please try again.",
		})
		return
	}

	// Box 반환을 보장하기 위한 defer
	defer func() {
		// Box 정리 (isolate --cleanup)
		cleanupCmd := exec.Command("isolate", "--cg", "--box-id", fmt.Sprintf("%d", boxID), "--cleanup")
		if output, err := cleanupCmd.CombinedOutput(); err != nil {
			log.Printf("Failed to cleanup box %d: %v, output: %s\n", boxID, err, output)
		} else {
			log.Printf("Box %d cleaned up\n", boxID)
		}

		// Box 재초기화 (isolate --init)
		initCmd := exec.Command("isolate", "--cg", "--box-id", fmt.Sprintf("%d", boxID), "--init")
		if output, err := initCmd.CombinedOutput(); err != nil {
			log.Printf("Failed to reinit box %d: %v, output: %s\n", boxID, err, output)
		} else {
			log.Printf("Box %d reinitialized: %s\n", boxID, strings.TrimSpace(string(output)))
		}

		// Box Pool에 반환
		s.boxPool <- boxID
		log.Printf("Box %d returned to pool\n", boxID)
	}()

	ctx := &ConnectionContext{
		conn:  conn,
		boxID: boxID,
	}

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Println("ReadJSON error:", err)
			break
		}

		switch msg.Type {
		case "code":
			err := handleCode(ctx, &msg)
			if err != nil {
				log.Println("handleCode error:", err)
				// 에러 발생 시 연결 종료
				conn.Close()
				return
			}

		case "input":
			if ctx.stdinPipe != nil {
				inputData := msg.Data
				if inputData == "\r" || inputData == "\n" {
					inputData = "\r\n"
				}
				_, writeErr := ctx.stdinPipe.Write([]byte(inputData))
				if writeErr != nil {
					log.Println("stdinPipe.Write error:", writeErr)
					sendJSON(conn, map[string]interface{}{
						"type":  "error",
						"error": fmt.Sprintf("stdin write error: %v", writeErr),
					})
					conn.Close()
					return
				}

				log.Println("input", inputData)

				if inputData != "\r\n" {
					sendJSON(conn, map[string]interface{}{
						"type": "echo",
						"data": inputData,
					})
				}
			}

		case "exit":
			sendJSON(conn, map[string]interface{}{
				"type": "exit",
				"data": "Process exit",
			})
			
			conn.Close()
			return

		default:
			sendJSON(conn, map[string]interface{}{
				"type":  "error",
				"error": "Unknown message type",
			})
		}
	}
}

// "code" 타입 메시지를 처리 (코드 파일 생성 → 컴파일 → 실행)
func handleCode(ctx *ConnectionContext, msg *Message) error {
	if _, ok := CompileOptions[msg.Language]; !ok {
		return fmt.Errorf("Unsupported language: %s", msg.Language)
	}

	filename := CompileOptions[msg.Language].Filename

	// isolate box 디렉토리 경로 구성
	boxDir := fmt.Sprintf("/var/local/lib/isolate/%d/box", ctx.boxID)
	
	// /code 디렉토리 생성 (filename이 /code/main.c 형식이므로)
	codeDir := fmt.Sprintf("%s/code", boxDir)
	if err := os.MkdirAll(codeDir, 0755); err != nil {
		sendJSON(ctx.conn, map[string]interface{}{
			"type":  "error",
			"error": fmt.Sprintf("Failed to create code directory: %v", err),
		})
		return err
	}

	filePath := fmt.Sprintf("%s%s", boxDir, filename) // filename은 /code/main.c 형식이므로 boxDir + filename

	// 코드 파일을 isolate box 디렉토리에 생성
	err := os.WriteFile(filePath, []byte(msg.Source), 0644)
	if err != nil {
		sendJSON(ctx.conn, map[string]interface{}{
			"type":  "error",
			"error": fmt.Sprintf("Failed to write file: %v", err),
		})
		return err
	}
	log.Printf("Box %d: Created file %s\n", ctx.boxID, filePath)

	// 컴파일 (isolate 내부에서)
	compileCmd := CompileOptions[msg.Language].CompileCmd
	if len(compileCmd) > 0 {
		output, compileErr := runCommandInIsolate(ctx.boxID, compileCmd)
		if compileErr != nil {
			sendJSON(ctx.conn, map[string]interface{}{
				"type":   "compile_error",
				"stderr": output,
			})
			// 컴파일 에러 발생 시 연결 종료
			ctx.conn.Close()
			return compileErr
		}

		sendJSON(ctx.conn, map[string]interface{}{
			"type":   "compile_success",
			"stdout": output,
		})
	}

	// 실행 (isolate 내부에서, 인터랙티브 모드)
	executeCmd := CompileOptions[msg.Language].ExecuteCmd
	if len(executeCmd) > 0 {
		if ctx.cmd != nil {
			_ = ctx.cmd.Process.Kill()
		}

		err := runInteractive(ctx, executeCmd)
		if err != nil {
			log.Println("runInteractive error:", err)
			// 실행 중 오류 발생 시 연결 종료
			ctx.conn.Close()
			return err
		}
	}

	return nil
}

// 명령어를 실행하고 결과(표준출력+표준에러)를 반환
func runCommand(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("no command to run")
	}

	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// runCommandInIsolate: isolate 내부에서 명령어를 실행하고 결과를 반환 (컴파일용)
func runCommandInIsolate(boxID int32, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("no command to run")
	}

	// isolate --cg --box-id {boxID} --run -- {command} {args...}
	isolateArgs := []string{
		"--cg",
		"--box-id", fmt.Sprintf("%d", boxID),
		"--run",
		"--",
	}
	isolateArgs = append(isolateArgs, args...)

	cmd := exec.Command("isolate", isolateArgs...)
	output, err := cmd.CombinedOutput()

	log.Printf("Box %d: Compile command: %v\n", boxID, args)
	if err != nil {
		log.Printf("Box %d: Compile error: %v, output: %s\n", boxID, err, string(output))
	}

	return string(output), err
}

// 프로세스를 실행하고, stdout/stderr를 실시간으로 웹소켓에 전송. stdinPipe는 ctx에 저장
func runInteractive(ctx *ConnectionContext, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command to run")
	}
	//cg를 사용하여 분리 (cgroup)
	// isolate --cg --box-id {boxID} --run -- {command} {args...}
	isolateArgs := []string{
		"--cg",
		"--box-id", fmt.Sprintf("%d", ctx.boxID),
		"--run",
		"--",
	}
	isolateArgs = append(isolateArgs, args...)

	log.Printf("Box %d: Starting execution: %v\n", ctx.boxID, args)

	cmd := exec.Command("isolate", isolateArgs...)
	ctx.cmd = cmd

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	ctx.stdinPipe = stdinPipe

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go streamOutput(ctx, stdoutPipe, "stdout")
	go streamOutput(ctx, stderrPipe, "stderr")

	// 프로세스 종료 대기
	go func() {
		waitErr := cmd.Wait()
		exitCode := 0
		if waitErr != nil {
			exitCode = cmd.ProcessState.ExitCode()
		} else {
			exitCode = cmd.ProcessState.ExitCode()
		}

		log.Printf("Box %d: Execution finished with exit code %d\n", ctx.boxID, exitCode)

		// 종료 메시지 전송
		sendJSON(ctx.conn, map[string]interface{}{
			"type":        "exit",
			"return_code": exitCode,
			"error":       fmt.Sprintf("%v", waitErr),
		})

		// 종료 후 stdinPipe 닫기 (cleanup은 wsHandler의 defer에서 처리)
		ctx.stdinPipe = nil
		ctx.cmd = nil
		ctx.conn.Close()
	}()

	return nil
}

// r(표준출력/표준에러)에서 데이터를 읽어, 실시간으로 웹소켓 전송
func streamOutput(ctx *ConnectionContext, r io.ReadCloser, streamType string) {
	defer r.Close()
	buf := make([]byte, 1024)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			line := string(buf[:n])
			ctx.mu.Lock()
			log.Println("output", line)
			sendJSON(ctx.conn, map[string]interface{}{
				"type": streamType,
				"data": line,
			})
			ctx.mu.Unlock()
		}

		if err != nil {
			break
		}
	}
}

// 웹소켓으로 JSON 전송
func sendJSON(conn *websocket.Conn, v interface{}) {
	if err := conn.WriteJSON(v); err != nil {
		log.Println("WriteJSON error:", err)
	}
}
