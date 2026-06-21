package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

const (
	VolcanoASRURL = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async"
	ResourceID    = "volc.seedasr.sauc.duration"

	ProtocolVersion = 0x1
	HeaderSize      = 0x1

	MsgTypeFullClient = 0x1
	MsgTypeAudioOnly  = 0x2
	MsgTypeFullServer = 0x9
	MsgTypeError      = 0xf

	SerialMethodNone = 0x0
	SerialMethodJSON = 0x1

	CompressionNone = 0x0

	FlagNoSequence    = 0x0
	FlagPositiveSeqNum = 0x1
	FlagLastPacket    = 0x2
	FlagLastNegSeqNum  = 0x3
)

var (
	AppID       = os.Getenv("VOLCANO_APP_ID")
	AccessToken = os.Getenv("VOLCANO_ACCESS_TOKEN")
	SecretKey   = os.Getenv("VOLCANO_SECRET_KEY")
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x4f) | 0x40
	b[7] = (b[7] & 0xbf) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:12]),
		hex.EncodeToString(b[12:16]))
}

func encodeHeader(msgType, flags, serialMethod, compression byte) []byte {
	h := byte((ProtocolVersion << 4) | HeaderSize)
	b1 := (msgType << 4) | flags
	b2 := (serialMethod << 4) | compression
	return []byte{h, b1, b2, 0}
}

func sendFullClientRequest(ws *websocket.Conn) error {
	req := map[string]interface{}{
		"app": map[string]interface{}{
			"appid": AppID,
		},
		"user": map[string]interface{}{
			"uid":        "desktop-ai-secretary",
			"did":        "device-001",
			"platform":   "Linux",
			"sdk_version": "1.0.0",
		},
		"audio": map[string]interface{}{
			"format":   "pcm",
			"rate":     16000,
			"bits":     16,
			"channel":  1,
			"language": "zh-CN",
		},
		"request": map[string]interface{}{
			"model_name":       "bigmodel",
			"enable_itn":        true,
			"enable_punc":       true,
			"enable_ddc":        false,
			"show_utterances":   true,
			"result_type":       "single",
			"show_speech_rate":  true,
		},
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal full request: %w", err)
	}

	header := encodeHeader(MsgTypeFullClient, FlagNoSequence, SerialMethodJSON, CompressionNone)

	payloadLen := make([]byte, 4)
	binary.BigEndian.PutUint32(payloadLen, uint32(len(payload)))

	msg := append(header, payloadLen...)
	msg = append(msg, payload...)

	err = ws.WriteMessage(websocket.BinaryMessage, msg)
	if err != nil {
		return fmt.Errorf("send full client request: %w", err)
	}

	log.Printf("[Volcano] Full client request sent, payload_len=%d", len(payload))
	if err != nil {
		return fmt.Errorf("send full client request: %w", err)
	}

	log.Printf("[Volcano] Full client request sent (text), payload_len=%d", len(payload))
	return nil
}

func decodeVolcanoResponse(msgType byte, data []byte) (text, utteranceText string, isComplete bool, err error) {
	if len(data) == 0 {
		return
	}

	// Audio packets (msg_type 0x1=AudioOnly) don't carry JSON results, just skip
	if msgType == MsgTypeAudioOnly {
		return
	}

	// AudioResult packets (msg_type 0x3) — raw JSON, no binary header
	if msgType == 0x3 {
		var resp map[string]interface{}
		if err = json.Unmarshal(data, &resp); err != nil {
			err = fmt.Errorf("unmarshal audio result: %w", err)
			return
		}
		if result, ok := resp["result"].(map[string]interface{}); ok {
			if textVal, ok := result["text"].(string); ok {
				text = textVal
			}
			if utts, ok := result["utterances"].([]interface{}); ok {
				for _, u := range utts {
					if uMap, ok := u.(map[string]interface{}); ok {
						if t, ok := uMap["text"].(string); ok {
							utteranceText = t
						}
					}
				}
			}
		}
		return
	}

	// Binary protocol: 4-byte header + 4-byte seq + 4-byte payload_len + payload
	if len(data) < 12 {
		err = fmt.Errorf("response too short: %d bytes", len(data))
		return
	}

	serialMethod := data[2] >> 4

	if msgType == MsgTypeError {
		if serialMethod == SerialMethodJSON && len(data) > 12 {
			err = fmt.Errorf("volcano error: %s", string(data[12:]))
		}
		return
	}

		if msgType != MsgTypeFullServer {
			return
		}

		isComplete = true

		offset := 8 // skip header(4) + seq(4)
	payloadSize := binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4
	if int(payloadSize)+offset > len(data) {
		return
	}
	payload := data[offset : offset+int(payloadSize)]

	var resp map[string]interface{}
	if err = json.Unmarshal(payload, &resp); err != nil {
		err = fmt.Errorf("unmarshal response: %w", err)
		return
	}

	if result, ok := resp["result"].(map[string]interface{}); ok {
		if textVal, ok := result["text"].(string); ok {
			text = textVal
		}
		if utts, ok := result["utterances"].([]interface{}); ok {
			for _, u := range utts {
				if uMap, ok := u.(map[string]interface{}); ok {
					if t, ok := uMap["text"].(string); ok {
						utteranceText = t
					}
				}
			}
		}
	}

	return
}

func volcanoDial() (*websocket.Conn, string, error) {
	resourceIDs := []string{
		ResourceID,
		"volc.bigasr.sauc.duration",
		"volc.bigasr.sauc.concurrent",
		"volcengine_streaming_common",
	}

	var lastErr error
	for _, rid := range resourceIDs {
		connectID := generateUUID()
		auth := buildVolcanoAuthHeaderFor(rid, connectID)

		header := http.Header{}
		header.Set("Authorization", auth)
		header.Set("X-Api-App-Key", AppID)
		header.Set("X-Api-Access-Key", AccessToken)
		header.Set("X-Api-Resource-Id", rid)
		header.Set("X-Api-Connect-Id", connectID)

		u, _ := url.Parse(VolcanoASRURL)
		conn, resp, err := websocket.DefaultDialer.Dial(u.String(), header)
		if err == nil {
			log.Printf("[Volcano] Connected with resource_id=%s, status=%s", rid, resp.Status)
			return conn, connectID, nil
		}
		if resp != nil {
			body, _ := readBody(resp)
			log.Printf("[Volcano] Tried resource_id=%s, status=%s, body=%s", rid, resp.Status, body)
			lastErr = fmt.Errorf("status=%s, body=%s", resp.Status, body)
		} else {
			lastErr = fmt.Errorf("%w", err)
		}
	}
	return nil, "", fmt.Errorf("all resource IDs failed, last: %w", lastErr)
}

func buildVolcanoAuthHeaderFor(rid, connectID string) string {
	authStr := fmt.Sprintf("GET /api/v3/sauc/bigmodel_async HTTP/1.1\r\n"+
		"Host: openspeech.bytedance.com\r\n"+
		"X-Api-App-Key: %s\r\n"+
		"X-Api-Access-Key: %s\r\n"+
		"X-Api-Resource-Id: %s\r\n"+
		"X-Api-Connect-Id: %s\r\n",
		AppID, AccessToken, rid, connectID)

	h := hmac.New(sha256.New, []byte(SecretKey))
	h.Write([]byte(authStr))
	mac := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(h.Sum(nil))

	return fmt.Sprintf(`HMAC256; access_token="%s"; mac="%s"; h="X-Api-App-Key,X-Api-Access-Key,X-Api-Resource-Id,X-Api-Connect-Id"`,
		AccessToken, mac)
}

func buildVolcanoAuthHeader() string {
	return buildVolcanoAuthHeaderFor(ResourceID, generateUUID())
}

func readBody(resp *http.Response) (string, error) {
	if resp.Body == nil {
		return "", nil
	}
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}

type BrowserMessage struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
}

type ASRResultPayload struct {
	Text           string    `json:"text"`
	UtteranceText  string    `json:"utterance_text,omitempty"`
	IsComplete     bool      `json:"is_complete"`
	LatencyMs      int64     `json:"latency_ms"`
	Timestamp      int64     `json:"timestamp"`
	RawResponse    string    `json:"raw_response"`
	Error          string    `json:"error,omitempty"`
}

func handleBrowserWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("[WS] Browser connected from %s", r.RemoteAddr)

	volcanoConn, connectID, err := volcanoDial()
	if err != nil {
		log.Printf("[Volcano] Dial failed: %v", err)
		conn.WriteJSON(map[string]interface{}{
			"type":    "error",
			"error":   fmt.Sprintf("连接火山引擎失败: %v", err),
			"payload": ASRResultPayload{Error: err.Error()},
		})
		return
	}
	defer volcanoConn.Close()

	// Local state for this session
	currentConnID := connectID

	if err := sendFullClientRequest(volcanoConn); err != nil {
		log.Printf("[Volcano] Send full request failed: %v", err)
		conn.WriteJSON(map[string]interface{}{
			"type":    "error",
			"error":   fmt.Sprintf("发送配置请求失败: %v", err),
			"payload": ASRResultPayload{Error: err.Error()},
		})
		return
	}

	// Goroutine: read volcano responses
	go func() {
		for {
			_, msg, err := volcanoConn.ReadMessage()
			if err != nil {
				log.Printf("[Volcano] Read error: %v", err)
				return
			}
			if len(msg) < 2 {
				continue
			}

			binMsgType := (msg[1] >> 4) & 0xf
			text, uttText, isComplete, err := decodeVolcanoResponse(binMsgType, msg)
			if err != nil {
				log.Printf("[Volcano] Decode error: %v", err)
				conn.WriteJSON(map[string]interface{}{
					"type": "error",
					"error": err.Error(),
					"payload": ASRResultPayload{
						RawResponse: string(msg),
						Error:       err.Error(),
					},
				})
				continue
			}

			if text == "" && uttText == "" {
				continue
			}

			rawJSON, _ := json.Marshal(msg)
			payload := ASRResultPayload{
				Text:          text,
				UtteranceText: uttText,
				IsComplete:    isComplete,
				LatencyMs:     0,
				Timestamp:     time.Now().UnixMilli(),
				RawResponse:  string(rawJSON),
			}

			conn.WriteJSON(map[string]interface{}{
				"type":    "asr_result",
				"payload": payload,
			})
		}
	}()

	sendPacket := func(data []byte, isLast bool) error {
		if currentConnID != connectID {
			return fmt.Errorf("stale connection")
		}
		var flags byte
		if isLast {
			flags = FlagLastPacket
		} else {
			flags = FlagNoSequence
		}
		header := encodeHeader(MsgTypeAudioOnly, flags, SerialMethodNone, CompressionNone)
		payloadLen := make([]byte, 4)
		binary.BigEndian.PutUint32(payloadLen, uint32(len(data)))
		msg := append(header, payloadLen...)
		msg = append(msg, data...)
		return volcanoConn.WriteMessage(websocket.BinaryMessage, msg)
	}

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[WS] Read error: %v", err)
			return
		}

		switch msgType {
		case websocket.BinaryMessage:
			if err := sendPacket(data, false); err != nil {
				log.Printf("[Volcano] Send error: %v", err)
				conn.WriteJSON(map[string]interface{}{
					"type":  "error",
					"error": fmt.Sprintf("发送音频到火山失败: %v", err),
				})
				return
			}

		case websocket.TextMessage:
			var bm map[string]interface{}
			if err := json.Unmarshal(data, &bm); err != nil {
				continue
			}
			if bm["type"] == "end" {
				log.Printf("[WS] End signal received, sending last packet")
				sendPacket([]byte{}, true)
			}
		}
	}
}

func main() {
	if AppID == "" || AccessToken == "" || SecretKey == "" {
		log.Fatal("请设置环境变量 VOLCANO_APP_ID, VOLCANO_ACCESS_TOKEN, VOLCANO_SECRET_KEY")
	}

	http.HandleFunc("/ws", handleBrowserWS)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		htmlPage(w)
	})

	addr := ":8080"
	log.Printf("===========================================")
	log.Printf("  火山引擎 ASR Demo 启动")
	log.Printf("  监听地址: http://localhost%s", addr)
	log.Printf("  WebSocket: ws://localhost:%s/ws", addr)
	log.Printf("===========================================")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func htmlPage(w http.ResponseWriter) {
	w.Write([]byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>火山引擎 ASR 实时识别 Demo</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
    background: #0a0e1a;
    color: #e2e8f0;
    min-height: 100vh;
    display: flex;
    flex-direction: column;
    align-items: center;
    padding: 40px 20px;
  }

  h1 {
    font-size: 24px;
    font-weight: 600;
    color: #60a5fa;
    margin-bottom: 8px;
    letter-spacing: 1px;
  }

  .subtitle {
    color: #64748b;
    font-size: 14px;
    margin-bottom: 32px;
  }

  .card {
    background: #111827;
    border: 1px solid #1e293b;
    border-radius: 16px;
    padding: 28px 32px;
    width: 100%;
    max-width: 700px;
    margin-bottom: 20px;
  }

  .card-title {
    font-size: 13px;
    font-weight: 600;
    color: #475569;
    text-transform: uppercase;
    letter-spacing: 1.5px;
    margin-bottom: 20px;
  }

  .status-bar {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 20px;
  }

  .status-dot {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: #334155;
    transition: background 0.3s;
  }

  .status-dot.connected { background: #22c55e; box-shadow: 0 0 8px #22c55e80; }
  .status-dot.recording { background: #ef4444; box-shadow: 0 0 8px #ef444480; animation: pulse 1.2s infinite; }
  .status-dot.error { background: #f97316; }

  @keyframes pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.5; }
  }

  .status-text {
    font-size: 14px;
    color: #94a3b8;
  }

  .btn-group {
    display: flex;
    gap: 12px;
    flex-wrap: wrap;
    margin-bottom: 24px;
  }

  button {
    padding: 12px 24px;
    border: none;
    border-radius: 10px;
    font-size: 15px;
    font-weight: 500;
    cursor: pointer;
    transition: all 0.2s;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .btn-primary {
    background: #3b82f6;
    color: white;
  }
  .btn-primary:hover:not(:disabled) { background: #2563eb; }
  .btn-primary:disabled { background: #1e3a5f; color: #475569; cursor: not-allowed; }

  .btn-danger {
    background: #dc2626;
    color: white;
  }
  .btn-danger:hover:not(:disabled) { background: #b91c1c; }
  .btn-danger:disabled { background: #3f1a1a; color: #6b3030; cursor: not-allowed; }

  .btn-secondary {
    background: #1e293b;
    color: #94a3b8;
    border: 1px solid #334155;
  }
  .btn-secondary:hover { background: #273549; }

  #transcript {
    background: #0d1117;
    border: 1px solid #1e293b;
    border-radius: 10px;
    padding: 20px 24px;
    min-height: 200px;
    max-height: 500px;
    overflow-y: auto;
    font-size: 15px;
    line-height: 2;
    color: #cbd5e1;
    cursor: text;
  }

  #transcript::-webkit-scrollbar { width: 6px; }
  #transcript::-webkit-scrollbar-track { background: #0d1117; }
  #transcript::-webkit-scrollbar-thumb { background: #334155; border-radius: 3px; }

  .cursor-blink {
    display: inline-block;
    width: 2px;
    height: 1em;
    background: #60a5fa;
    vertical-align: text-bottom;
    animation: blink 1s step-end infinite;
    margin-left: 1px;
  }

  @keyframes blink {
    0%, 100% { opacity: 1; }
    50% { opacity: 0; }
  }

  .final-text { color: #cbd5e1; }
  .interim-text { color: #94a3b8; font-style: italic; }
  .segment-sep { color: #1e293b; margin: 0 4px; }
  .error-text { color: #f87171; }

  .metrics {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
    gap: 10px;
    margin-top: 12px;
  }

  .metric {
    background: #0d1117;
    border: 1px solid #1e293b;
    border-radius: 10px;
    padding: 14px;
    text-align: center;
  }

  .metric-value {
    font-size: 22px;
    font-weight: 700;
    color: #60a5fa;
    font-variant-numeric: tabular-nums;
  }

  .metric-label {
    font-size: 12px;
    color: #475569;
    margin-top: 4px;
  }

  .raw-section {
    margin-top: 16px;
  }

  .raw-toggle {
    font-size: 12px;
    color: #475569;
    cursor: pointer;
    user-select: none;
    padding: 6px 0;
  }

  .raw-toggle:hover { color: #60a5fa; }

  #rawOutput {
    display: none;
    margin-top: 8px;
  }

  #rawOutput.visible { display: block; }

  #rawOutput pre {
    background: #0d1117;
    border: 1px solid #1e293b;
    border-radius: 8px;
    padding: 12px;
    font-size: 11px;
    color: #64748b;
    overflow-x: auto;
    max-height: 250px;
    overflow-y: auto;
    white-space: pre-wrap;
    word-break: break-all;
  }

  .hint {
    font-size: 13px;
    color: #475569;
    text-align: center;
    margin-top: 8px;
  }

  .info-tag {
    display: inline-block;
    font-size: 10px;
    padding: 2px 6px;
    border-radius: 4px;
    background: #1e293b;
    color: #64748b;
    margin-left: 8px;
    font-family: monospace;
  }

  .clear-link {
    font-size: 13px;
    color: #475569;
    text-decoration: none;
    cursor: pointer;
    float: right;
  }
  .clear-link:hover { color: #ef4444; }
</style>
</head>
<body>

<h1>&#127917; 火山引擎 ASR 实时识别</h1>
<p class="subtitle">浏览器麦克风 &rarr; WebSocket &rarr; Go 后端 &rarr; 火山引擎 &rarr; 实时转写</p>

<div class="card">
  <div class="card-title">&#128270; 连接状态</div>
  <div class="status-bar">
    <div class="status-dot" id="statusDot"></div>
    <span class="status-text" id="statusText">未连接</span>
  </div>
  <div class="btn-group">
    <button class="btn-primary" id="btnConnect" onclick="connect()">
      <span>&#128225;</span> 连接
    </button>
    <button class="btn-primary" id="btnStart" onclick="startRecording()" disabled>
      <span>&#127908;</span> 开始识别
    </button>
    <button class="btn-danger" id="btnStop" onclick="stopRecording()" disabled>
      <span>&#9632;</span> 停止
    </button>
    <button class="btn-secondary" id="btnDisconnect" onclick="disconnect()" disabled>
      <span>&#10006;</span> 断开
    </button>
  </div>
  <p class="hint" id="hint">点击「连接」后，授权麦克风权限，再点击「开始识别」</p>
</div>

<div class="card">
  <div class="card-title">
    &#128221; 实时转写
    <a class="clear-link" onclick="clearTranscript()">&#128465; 清空</a>
  </div>
  <div id="transcript">
    <span style="color:#334155;font-size:14px;">等待开始识别...</span>
  </div>
  <div class="metrics">
    <div class="metric">
      <div class="metric-value" id="metricDuration">0s</div>
      <div class="metric-label">识别时长</div>
    </div>
    <div class="metric">
      <div class="metric-value" id="metricPackets">0</div>
      <div class="metric-label">音频包数</div>
    </div>
    <div class="metric">
      <div class="metric-value" id="metricLatency">--</div>
      <div class="metric-label">延迟 (ms)</div>
    </div>
  </div>
</div>

<div class="card">
  <div class="card-title">&#128206; 原始返回</div>
  <div class="raw-toggle" onclick="toggleRaw()">&#9656; 点击展开原始火山引擎返回 <span id="rawCount"></span></div>
  <div id="rawOutput">
    <pre id="rawContent">暂无数据...</pre>
  </div>
</div>

<script>
let ws = null;
let audioContext = null;
let processor = null;
let micStream = null;

let isConnected = false;
let isRecording = false;
let startTime = null;
let packetCount = 0;

// Streaming text state
let sentences = [];       // completed sentences
let currentText = "";     // current streaming text
let lastLen = 0;          // previous text length for sentence boundary detection
let rawCount = 0;

const transcriptEl = document.getElementById('transcript');
const statusDot = document.getElementById('statusDot');
const statusText = document.getElementById('statusText');
const hint = document.getElementById('hint');
const rawContent = document.getElementById('rawContent');
const rawOutput = document.getElementById('rawOutput');
const rawCountEl = document.getElementById('rawCount');
const metricDuration = document.getElementById('metricDuration');
const metricLatency = document.getElementById('metricLatency');
const metricPackets = document.getElementById('metricPackets');

function setStatus(state, text) {
  statusDot.className = "status-dot " + (state === "idle" ? "" : state);
  statusText.textContent = text;
}

function setButtons(connected, recording) {
  document.getElementById("btnConnect").disabled = connected;
  document.getElementById("btnStart").disabled = !connected || recording;
  document.getElementById("btnStop").disabled = !recording;
  document.getElementById("btnDisconnect").disabled = !connected;
}

function connect() {
  const wsHost = location.host;
  ws = new WebSocket("ws://" + wsHost + "/ws");
  ws.binaryType = "arraybuffer";

  ws.onopen = () => {
    console.log("[WS] Connected");
    isConnected = true;
    setStatus("connected", "已连接火山引擎");
    setButtons(true, false);
    hint.textContent = "已连接，点击「开始识别」";
  };

  ws.onmessage = (evt) => {
    try {
      const msg = JSON.parse(evt.data);
      if (msg.type === "asr_result") {
        handleASRResult(msg.payload);
      } else if (msg.type === "error") {
        handleError(msg.error, msg.payload);
      }
    } catch(e) {
      console.error("[WS] Parse error:", e);
    }
  };

  ws.onclose = () => {
    console.log("[WS] Disconnected");
    isConnected = false;
    isRecording = false;
    stopMedia();
    setStatus("idle", "连接已断开");
    setButtons(false, false);
    hint.textContent = "连接已断开";
  };

  ws.onerror = (err) => {
    console.error("[WS] Error:", err);
    setStatus("error", "连接错误");
  };
}

function disconnect() {
  if (isRecording) stopRecording();
  if (ws) ws.close();
  ws = null;
  isConnected = false;
  setStatus("idle", "未连接");
  setButtons(false, false);
  hint.textContent = "点击「连接」重新开始";
}

function handleASRResult(payload) {
  packetCount++;
  metricPackets.textContent = packetCount;

  if (payload.latency_ms) {
    metricLatency.textContent = Math.round(payload.latency_ms);
  }

  if (startTime) {
    const elapsed = Math.floor((Date.now() - startTime) / 1000);
    metricDuration.textContent = elapsed + "s";
  }

  const text = (payload.utterance_text || payload.text || "").trim();
  if (!text) return;

  rawCount++;
  rawCountEl.textContent = "(" + rawCount + " 条)";

  const textLen = text.length;
  const THRESHOLD = 0.6;
  const MIN_OLD = 8;
  const MIN_NEW = 5;

  if (textLen < MIN_NEW && lastLen >= MIN_OLD && textLen < lastLen * THRESHOLD) {
    const prev = currentText.trim();
    if (prev) {
      sentences.push(prev);
      console.log("[SENTENCE SAVED] " + prev + " (len=" + prev.length + ") sentences=" + sentences.length);
    }
  } else if (textLen < lastLen * 0.6) {
    const prev = currentText.trim();
    if (prev) {
      sentences.push(prev);
      console.log("[SENTENCE SAVED fallback] " + prev + " (len=" + prev.length + ") sentences=" + sentences.length);
    }
  }

  currentText = text;
  lastLen = textLen;

  renderTranscript();

  try {
    const raw = JSON.parse(payload.raw_response);
    appendRaw(JSON.stringify(raw, null, 2));
  } catch(e) {
    appendRaw(payload.raw_response);
  }
}

function renderTranscript() {
  let html = sentences.map(s => '<span class="final-text">' + escapeHtml(s) + '</span>').join('<span class="segment-sep">|</span> ');

  if (currentText) {
    const lastSentence = sentences[sentences.length - 1] || "";
    const isNew = !lastSentence || !currentText.startsWith(lastSentence);
    if (isNew) {
      if (html) html += ' ';
      html += '<span class="interim-text">' + escapeHtml(currentText) + '<span class="cursor-blink"></span></span>';
    } else {
      const suffix = currentText.slice(lastSentence.length);
      if (html) html += ' ';
      html += '<span class="interim-text">' + escapeHtml(suffix) + '<span class="cursor-blink"></span></span>';
    }
  }

  transcriptEl.innerHTML = html || '<span style="color:#334155;font-size:14px;">等待开始识别...</span>';
  transcriptEl.scrollTop = transcriptEl.scrollHeight;
}

function handleError(errMsg, payload) {
  transcriptEl.innerHTML = '<span class="error-text">&#9888; ' + escapeHtml(errMsg) + '</span>';
  appendRaw("[ERROR] " + errMsg);
  if (payload && payload.raw_response) {
    try {
      appendRaw(JSON.stringify(JSON.parse(payload.raw_response), null, 2));
    } catch(e) {
      appendRaw(payload.raw_response);
    }
  }
}

function appendRaw(text) {
  const existing = rawContent.textContent;
  if (existing === "暂无数据...") {
    rawContent.textContent = text;
  } else {
    rawContent.textContent = existing + "\n\n" + "=".repeat(40) + "\n\n" + text;
  }
}

function escapeHtml(str) {
  const div = document.createElement("div");
  div.textContent = str;
  return div.innerHTML;
}

async function startRecording() {
  try {
    setStatus("recording", "识别中...");
    setButtons(true, true);
    hint.textContent = "正在识别，请说话...";

    const stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: 1,
        sampleRate: 16000,
        echoCancellation: false,
        noiseSuppression: false,
        autoGainControl: false,
      }
    });

    audioContext = new AudioContext({ sampleRate: 16000 });
    micStream = audioContext.createMediaStreamSource(stream);
    processor = audioContext.createScriptProcessor(4096, 1, 1);

    let lastSendTime = 0;
    const targetInterval = 160;

    processor.onaudioprocess = (e) => {
      if (!isRecording) return;

      const now = Date.now();
      if (now - lastSendTime < targetInterval) return;
      lastSendTime = now;

      const inputData = e.inputBuffer.getChannelData(0);

      const pcmData = new Int16Array(inputData.length);
      for (let i = 0; i < inputData.length; i++) {
        const s = Math.max(-1, Math.min(1, inputData[i]));
        pcmData[i] = s < 0 ? s * 0x8000 : s * 0x7FFF;
      }

      const buf = new ArrayBuffer(pcmData.length * 2);
      const view = new DataView(buf);
      for (let i = 0; i < pcmData.length; i++) {
        view.setInt16(i * 2, pcmData[i], true);
      }

      if (ws && ws.readyState === WebSocket.OPEN) {
        packetCount++;
        metricPackets.textContent = packetCount;
        ws.send(buf);
      }
    };

    micStream.connect(processor);
    processor.connect(audioContext.destination);
    stream.getTracks().forEach(t => t.enabled = true);

    isRecording = true;
    startTime = Date.now();

    const durationTimer = setInterval(() => {
      if (startTime && isRecording) {
        const elapsed = Math.floor((Date.now() - startTime) / 1000);
        metricDuration.textContent = elapsed + "s";
      } else {
        clearInterval(durationTimer);
      }
    }, 1000);

  } catch(err) {
    console.error("[Mic] Error:", err);
    setStatus("error", "麦克风权限错误: " + err.message);
    setButtons(true, false);
    hint.textContent = "请允许麦克风权限后重试";
  }
}

function stopRecording() {
  isRecording = false;
  setStatus("connected", "已连接");
  setButtons(true, false);
  hint.textContent = "识别已停止，可以继续或断开连接";

  stopMedia();

  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "end" }));
  }
}

function stopMedia() {
  if (processor) { try { processor.disconnect(); } catch(e){} processor = null; }
  if (micStream) { try { micStream.disconnect(); } catch(e){} micStream = null; }
  if (audioContext) { try { audioContext.close(); } catch(e){} audioContext = null; }
  if (micStream && micStream.getTracks) {
    micStream.getTracks().forEach(t => t.stop());
  }
}

function clearTranscript() {
  transcriptEl.innerHTML = '<span style="color:#334155;font-size:14px;">等待开始识别...</span>';
  sentences = [];
  currentText = "";
  lastLen = 0;
  rawCount = 0;
  packetCount = 0;
  metricPackets.textContent = "0";
  metricDuration.textContent = "0s";
  metricLatency.textContent = "--";
  startTime = null;
  rawContent.textContent = "暂无数据...";
  rawCountEl.textContent = "";
}

function toggleRaw() {
  rawOutput.classList.toggle("visible");
  const toggle = rawOutput.previousElementSibling;
  if (rawOutput.classList.contains("visible")) {
    toggle.innerHTML = '&#9662; 点击收起原始返回 <span id="rawCount"></span>';
    toggle.querySelector("span").textContent = "(" + rawCount + " 条)";
  } else {
    toggle.innerHTML = '&#9656; 点击展开原始火山引擎返回 <span id="rawCount"></span>';
    toggle.querySelector("span").textContent = "(" + rawCount + " 条)";
  }
}
</script>
</body>
</html>
`))
}
