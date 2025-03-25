package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/gorilla/websocket"
)

type Message struct {
	Type       string `json:"type"`        // "code", "input", "echo" 등
	Language   string `json:"language"`    // c, cpp, java, go, python, ...
	Filename   string `json:"filename"`    // 예) "hello.c"
	Source     string `json:"source"`      // 소스코드
	CompileCmd string `json:"compile_cmd"` // 예) "gcc hello.c -o hello"
	Command    string `json:"command"`     // 예) "./hello"
	Data       string `json:"data"`        // "input" 메시지에서 사용 (stdin에 보낼 내용)
}

// ConnectionContext: 한 WebSocket 커넥션에서 실행 중인 프로세스 정보를 저장
type ConnectionContext struct {
	conn      *websocket.Conn
	cmd       *exec.Cmd
	stdinPipe io.WriteCloser

	// 표준출력/표준에러를 중복해서 웹소켓에 보내지 않도록 보호하는 뮤텍스 등
	mu sync.Mutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func main() {
	http.HandleFunc("/ws", wsHandler)

	addr := ":8000"
	log.Printf("WebSocket server running on %s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	ctx := &ConnectionContext{conn: conn}

	// TODO: remove me
	// this is for testing purpose
	sendJSON(ctx.conn, map[string]interface{}{
		"type": "stdout",
		"data": "Hi, this is a message from server",
	})

	// create /var/log/iris-runner/hi.txt file
	err = os.WriteFile("/var/log/iris-runner/hi.txt", []byte("hi"), 0644)
	if err != nil {
		panic(err)
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
	filename := msg.Filename
	if filename == "" {
		filename = "Main.c"
	}

	// TODO(jaemin): remove this hack
	// this code is for running c code in docker sandbox
	// only for testing purpose
	if msg.Language == "c" {
		filename = "main.c"
	}

	// 코드 파일 생성
	err := os.WriteFile(filename, []byte(msg.Source), 0644)
	if err != nil {
		sendJSON(ctx.conn, map[string]interface{}{
			"type":  "error",
			"error": fmt.Sprintf("Failed to write file: %v", err),
		})
		return err
	}

	// 컴파일
	if msg.CompileCmd != "" {
		compileArgs := strings.Split(msg.CompileCmd, " ")
		output, compileErr := runCommand(compileArgs)
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

	if msg.Command != "" {
		if ctx.cmd != nil {
			_ = ctx.cmd.Process.Kill()
		}

		err := runInSandbox(ctx, msg.Language)
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

// 프로세스를 실행하고, stdout/stderr를 실시간으로 웹소켓에 전송. stdinPipe는 ctx에 저장
func runInteractive(ctx *ConnectionContext, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command to run")
	}

	cmd := exec.Command(args[0], args[1:]...)
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

		// 종료 메시지 전송
		sendJSON(ctx.conn, map[string]interface{}{
			"type":        "exit",
			"return_code": exitCode,
			"error":       fmt.Sprintf("%v", waitErr),
		})

		// 종료 후 stdinPipe 닫기
		ctx.stdinPipe = nil
		ctx.cmd = nil
		ctx.conn.Close()
	}()

	return nil
}

func runInSandbox(ctx *ConnectionContext, language string) error {
	ctx_bg := context.Background()
	uniqueID := uuid.New().String()

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()

	if language != "c" {
		return fmt.Errorf("only c language is supported")
	}

	imageName := "sandbox:" + uniqueID
	BuildImage(cli, imageName, "sandbox-c.Dockerfile", ".")

	containerConfig := &container.Config{
		Image: imageName,
		Tty:   false,
	}
	resp, err := cli.ContainerCreate(ctx_bg, containerConfig, nil, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %v", err)
	}

	containerID := resp.ID
	defer func() {
		cli.ContainerRemove(ctx_bg, containerID, container.RemoveOptions{Force: true})
	}()

	if err := cli.ContainerStart(ctx_bg, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	}

	out, err := cli.ContainerLogs(ctx_bg, resp.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
	})
	if err != nil {
		return fmt.Errorf("failed to get container logs: %v", err)
	}
	defer out.Close()

	go streamOutput(ctx, out, "stdout")

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
