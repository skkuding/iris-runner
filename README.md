## **아키텍처**
<img width="1630" alt="image" src="https://github.com/user-attachments/assets/62ba5dd5-87a9-4a6c-8866-9e854b8c0233" />

## **프론트엔드 로직**

1. 클라이언트가 RUN 버튼을 클릭하면 웹소켓 연결을 수립합니다.
2. 웹소켓 연결을 성공하면 에디터의 코드를 code 타입 메시지로 전송합니다.
3. 실행 중인 프로그램이 입력을 기다릴 때, 사용자 입력을 input 타입 메시지로 전송합니다.
4. 백엔드로부터 받은 stdout, stderr, compile_success, compile_error, exit 등의 메시지를 화면에 표시합니다.
5. 프로그램 종료 시 웹소켓 연결이 종료되며, 새로운 실행을 위해 다시 연결해야 합니다. (다시 RUN 버튼 클릭)

## **백엔드 로직**

1. /ws 경로로 들어온 HTTP 요청을 WebSocket 연결로 업그레이드합니다.
2. ConnectionContext를 생성하여 연결 상태를 관리합니다.
3. 무한 루프를 통해 클라이언트로부터 JSON 형식의 메시지를 계속 수신합니다.
4. 메시지 타입에 따라 다른 처리를 수행합니다.
    - code 타입: 코드 파일 생성, 컴파일, 실행을 처리하는 handleCode 함수 호출
    - input 타입: 실행 중인 프로세스의 표준 입력에 데이터 전달
    - 기타 타입: 오류 메시지 응답
5. 실행 결과(stdout, stderr)를 실시간으로 클라이언트에게 전송합니다.
6. 프로그램 종료 시 exit 메시지와 함께 종료 코드를 전송합니다.

## **쿠버네티스 아키텍처**

1. 클라이언트가 요청할 때 독립적인 Runner Pod이 생성됩니다.
2. 컨테이너별 리소스 제한 설정 (hard limit: 0.5v CPU, 0.5 GiB / soft limit: 0.2vCPU, 0.2GiB)
3. 새로 생성된 Pod이 클라이언트와 WebSocket 연결이 이루어집니다.
4. 이 때 연결은 Pod Manager가 중재합니다. (clientConn, podConn)
5. Pod Manager는 다음과 같은 역할을 수행합니다:
    - Pod 생성 및 생명주기 관리
    - Pod 준비 상태 모니터링 (최대 90초 대기)
    - 클라이언트와 Pod 간 WebSocket 메시지 중계
    - 연결 종료 시 Pod 자동 삭제 (리소스 정리)

## **시스템 구성 요소**

1. **프론트엔드**:
    - HTML/JavaScript 기반 웹 인터페이스
    - CodeMirror를 사용한 코드 에디터
    - Xterm.js를 사용한 터미널 에뮬레이터
    - WebSocket을 통한 실시간 통신
2. **Pod Manager**:
    - WebSocket 프록시 역할
    - RBAC 권한으로 Pod 생성/삭제 권한 보유
3. **Runner Pod**:
    - 사용자 코드 실행을 위한 격리된 환경
    - 다양한 프로그래밍 언어 지원 (C, C++, Python, Go, Java, JavaScript)
    - WebSocket 서버 내장
    - 컨테이너 리소스 제한으로 서버 안전성 보장
4. **Nginx**:
    - 프론트엔드 서비스와 Pod Manager 서비스로 라우팅
    - WebSocket 프로토콜 지원
    - 10분 동안 웹소켓 통신없으면 연결 종료

## **네트워크 흐름**

- 클라이언트 → Nginx → Pod Manager → Runner Pod
- Runner Pod → Pod Manager → Nginx → 클라이언트

## **메시지 프로토콜**

1. **클라이언트 → 서버**:
    
    ```json
    {
      "type": "code",
      "language": "c",
      "filename": "main.c",
      "source": "int main() { ... }",
      "compile_cmd": "gcc main.c -o main",
      "command": "./main"
    }
    
    ```
    
    또는
    
    ```json
    {
      "type": "input",
      "data": "사용자 입력 데이터"
    }
    
    ```
    
2. **서버 → 클라이언트**:
    
    ```json
    {
      "type": "stdout|stderr|compile_success|compile_error|exit|echo",
      "data": "출력 데이터",
      "stderr": "컴파일 오류 메시지",
      "return_code": 0
    }
    
    ```
