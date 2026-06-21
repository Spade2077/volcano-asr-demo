# Volcano ASR Demo

基于字节跳动火山引擎 BigModel ASR 的实时语音识别 Demo，通过浏览器麦克风实时转写为文字。

## 功能

- 浏览器麦克风实时采集音频
- 浏览器端麦克风实时播放
- WebSocket 实时传输至后端
- 实时显示识别结果
- 完整的原始数据包日志

## 配置

从 `.env.example` 复制一份 `.env`，填入火山引擎的凭证：

```bash
cp .env.example .env
```

```env
VOLCANO_APP_ID=your_app_id
VOLCANO_ACCESS_TOKEN=your_access_token
VOLCANO_SECRET_KEY=your_secret_key
```

## 运行

```bash
go build -o volcano-asr-demo.exe .
./volcano-asr-demo.exe
```

然后打开 http://localhost:8080

## 环境变量说明

| 变量 | 说明 |
|------|------|
| `VOLCANO_APP_ID` | 火山引擎应用 ID |
| `VOLCANO_ACCESS_TOKEN` | Access Token |
| `VOLCANO_SECRET_KEY` | Secret Key |

凭证请前往 [火山引擎控制台](https://console.volcengine.com) 获取。
