package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const LogFile = "/data/local/tmp/bridge.log"
const LogMax = 1024 * 1024
const LLMPort = "8082"
const RestartEvery = 10

var logMu sync.Mutex
var reqCount int

type LMResponse struct {
	Content string `json:"content"`
	Done    bool   `json:"done"`
}

func logf(format string, args ...interface{}) {
	t := time.Now().Format("2006/01/02 15:04:05")
	msg := fmt.Sprintf("[%s] %s\n", t, fmt.Sprintf(format, args...))
	logMu.Lock()
	defer logMu.Unlock()
	if fi, _ := os.Stat(LogFile); fi != nil && fi.Size() > LogMax {
		os.Rename(LogFile, LogFile+".old")
	}
	f, err := os.OpenFile(LogFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err == nil {
		f.WriteString(msg)
		f.Close()
	}
}

func tgAPI(method, data string) ([]byte, error) {
	return exec.Command("/system/bin/curl", "-s", "--connect-timeout", "5", "--max-time", "10",
		"-d", data,
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", TGToken, method)).Output()
}

func tgEscape(s string) string {
	out := make([]byte, 0, len(s)*3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
		} else if c == ' ' {
			out = append(out, '+')
		} else {
			out = append(out, '%', hext(c>>4), hext(c&0x0f))
		}
	}
	return string(out)
}

func hext(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'A' + n - 10
}

func sendTG(msg string) {
	for len(msg) > 4000 {
		tgAPI("sendMessage",
			fmt.Sprintf("chat_id=%s&text=%s&parse_mode=HTML", TGChatID, tgEscape(msg[:4000])))
		msg = msg[4000:]
	}
	tgAPI("sendMessage",
		fmt.Sprintf("chat_id=%s&text=%s&parse_mode=HTML", TGChatID, tgEscape(msg)))
}

func sendKB(chatID, msg string) {
	buttons := `{"inline_keyboard":[[` +
		`{"text":"/ai","switch_inline_query_current_chat":"/ai "},` +
		`{"text":"/fix","switch_inline_query_current_chat":"/fix "}` +
		`],[` +
		`{"text":"/shell","switch_inline_query_current_chat":"/shell "},` +
		`{"text":"/status","switch_inline_query_current_chat":"/status"},` +
		`{"text":"/log","switch_inline_query_current_chat":"/log"}` +
		`],[` +
		`{"text":"🔄 deauthd","switch_inline_query_current_chat":"/restart_deauthd"}` +
		`]]}`
	tgAPI("sendMessage",
		fmt.Sprintf("chat_id=%s&text=%s&parse_mode=HTML&reply_markup=%s",
			chatID, tgEscape(msg), tgEscape(buttons)))
}

func tgSendFile(chatID, path string) {
	exec.Command("/system/bin/sh", "-c",
		"exec /system/bin/curl -s --connect-timeout 5 --max-time 15 -F document=@"+
			path+" 'https://api.telegram.org/bot"+TGToken+"/sendDocument?chat_id="+chatID+"'").Run()
}

func getSysInfo() string {
	out1, _ := exec.Command("su", "-c", "cat /sys/class/thermal/thermal_zone*/temp 2>/dev/null | head -3 | tr '\\n' ' '").Output()
	out2, _ := exec.Command("su", "-c", "dumpsys battery 2>/dev/null | grep -E 'level|temperature|status' | paste -sd' '").Output()
	out3, _ := exec.Command("su", "-c", "free -m 2>/dev/null | grep Mem | awk '{print $3\"M/\"$2\"M\"}'").Output()
	out4, _ := exec.Command("su", "-c", "df -h /data 2>/dev/null | tail -1 | awk '{print $3\"/\"$2\" (\"$5\")\"}'").Output()
	return fmt.Sprintf("  [temps]: %s\n  [battery]: %s\n  [memory]: %s\n  [disk]: %s\n",
		strings.TrimSpace(string(out1)), strings.TrimSpace(string(out2)), strings.TrimSpace(string(out3)), strings.TrimSpace(string(out4)))
}

func queryLLM(system, user string) string {
	prompt := fmt.Sprintf("<|im_start|>system\n%s<|im_end|>\n<|im_start|>user\n%s<|im_end|>\n<|im_start|>assistant\n", system, user)
	body := fmt.Sprintf(`{"prompt":"%s","max_tokens":100,"temperature":0.3,"top_p":0.9,"stop":["<|im_end|>"]}`, jsonEscape(prompt))
	out, err := exec.Command("/system/bin/curl", "-s", "--connect-timeout", "5", "--max-time", "60",
		"-X", "POST", fmt.Sprintf("http://127.0.0.1:%s/v1/completions", LLMPort),
		"-H", "Content-Type: application/json",
		"-d", body).Output()
	if err != nil {
		return ""
	}
	var res struct {
		Choices []struct { Text string `json:"text"` } `json:"choices"`
	}
	json.Unmarshal(out, &res)
	if len(res.Choices) == 0 { return "" }
	return strings.TrimSpace(res.Choices[0].Text)
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

func isGarbage(s string) bool {
	return strings.Contains(s, "@@@@@") || strings.Count(s, "@") > 5
}

func restartLLM() {
	logf("restarting LLM server")
	exec.Command("su", "-c", "killall -9 llama-server 2>/dev/null; sleep 1").Run()
	exec.Command("su", "-c",
		"cd /data/local/tmp/llama-vk && nohup env LD_LIBRARY_PATH=. ./llama-server "+
			"-m /data/local/tmp/qwen_15.gguf -ngl 99 --no-mmap -c 1024 "+
			"--port 8082 --host 127.0.0.1 --no-kv-offload --cache-ram 0 "+
			"> /data/local/tmp/llm.log 2>&1 &").Run()
	time.Sleep(10 * time.Second)
	logf("server restarted")
}

func aiTask(task string) string {
	sys := getSysInfo()
	system := fmt.Sprintf("Ты — AI-ассистент на Android телефоне (Realme GT, Snapdragon 888+).\nДанные системы:\n%s\nОтвечай кратко (2-3 предложения) по-русски. Не выдумывай цифры.", sys)

	// First attempt
	result := queryLLM(system, task)
	if result != "" && !isGarbage(result) {
		return result
	}
	if isGarbage(result) {
		logf("garbage detected, restarting and retrying")
	}
	// Restart and retry
	restartLLM()
	result = queryLLM(system, task)
	if result != "" && !isGarbage(result) {
		return result
	}
	return "❌ Не удалось получить ответ."
}

func main() {
	logf("bridge started")
	sendKB(TGChatID, "🤖 <b>Bridge</b> запущен (локальный LLM)")
	offset := 0
	pendingCmd := ""

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-sigCh:
			logf("bridge stopped")
			return
		default:
		}
		time.Sleep(2 * time.Second)

		cmd := exec.Command("/system/bin/curl", "-s", "--connect-timeout", "5", "--max-time", "10",
			fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d", TGToken, offset))
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) < 10 {
			continue
		}

		var res struct {
			OK     bool `json:"ok"`
			Result []struct {
				UpdateID int `json:"update_id"`
				Message  *struct {
					Chat struct{ ID int } `json:"chat"`
					Text string `json:"text"`
				} `json:"message"`
			} `json:"result"`
		}
		json.Unmarshal(out, &res)
		if !res.OK {
			continue
		}

		for _, u := range res.Result {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Chat.ID != 619377851 {
				continue
			}
			text := strings.TrimSpace(u.Message.Text)
			text = strings.TrimPrefix(text, "@ai_network2_bot ")
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}

			// ── Pending command input ──
			if pendingCmd != "" && !strings.HasPrefix(text, "/") {
				cmdName := pendingCmd
				pendingCmd = ""
				tStart := time.Now()
				switch cmdName {
				case "ai":
					logf("CMD /ai: %q", text)
					sendTG(fmt.Sprintf("🧠 Думаю: <i>%s</i>", text))
					result := aiTask(text)
					if len(result) > 3500 { result = result[:3500] + "\n..." }
					logf("CMD /ai: done (%s)", time.Since(tStart))
					sendKB(TGChatID, fmt.Sprintf("🤖 <b>Ответ:</b>\n%s", result))
					reqCount++
					if reqCount >= RestartEvery { reqCount = 0; go restartLLM() }
				case "fix":
					logf("CMD /fix: %q", text)
					sendTG(fmt.Sprintf("🔧 Выполняю: <i>%s</i>", text))
					result := aiTask(text)
					if len(result) > 3500 { result = result[:3500] + "\n..." }
					logf("CMD /fix: done (%s)", time.Since(tStart))
					sendKB(TGChatID, fmt.Sprintf("🔧 <b>Результат:</b>\n%s", result))
					reqCount++
					if reqCount >= RestartEvery { reqCount = 0; go restartLLM() }
				}
				continue
			}

			switch {
			case text == "/start":
				logf("CMD /start")
				sendKB(TGChatID, "🤖 <b>Bridge</b> (локальный LLM)\n\n/ai — вопрос к 1.5B\n/fix — задача для 1.5B\n/shell — быстрая команда (0.5B)\n/status — состояние\n/log — лог\n/restart_deauthd — перезапустить deauthd")

			case text == "/status":
				tStart := time.Now()
				suOut, _ := exec.Command("su", "-c",
					"echo 'deauthd: '$(pidof deauthd); echo 'bridge: '$$; echo 'llm: '$(cat /data/local/tmp/llm.pid 2>/dev/null); df -h /data | tail -1").Output()
				upt, _ := exec.Command("su", "-c", "cat /proc/uptime | awk '{print int($1/3600)\"h\"}'").Output()
				logf("CMD /status → OK (%s)", time.Since(tStart))
				sendKB(TGChatID, fmt.Sprintf("📊 Статус:\n<code>%suptime: %s</code>", string(suOut), string(upt)))

			case text == "/log":
				logf("CMD /log")
				tgSendFile(TGChatID, LogFile)

			case text == "/restart_deauthd":
				tStart := time.Now()
				sendTG("⏳ Перезапускаю deauthd...")
				exec.Command("su", "-c", "killall -9 deauthd 2>/dev/null; sleep 1;"+
					"nohup /data/local/tmp/deauthd -i wlan0 -w c0:b3:c8:97:ec:f6 "+
					"-w b2:e0:37:90:80:eb -w d6:1d:6c:aa:59:ec "+
					"-w 78:3f:4d:c8:c4:69 -w 3c:0b:4f:5e:01:8c "+
					"-w 44:29:1e:b6:9b:17 -w da:98:03:f8:d4:3a "+
					"--log /data/local/tmp/deauthd.log "+
					"--telegram "+DeauthdTG+" >/dev/null 2>&1 &").Run()
				time.Sleep(2 * time.Second)
				pid, _ := exec.Command("su", "-c", "pidof deauthd").Output()
				logf("CMD /restart_deauthd → PID=%s (%s)", strings.TrimSpace(string(pid)), time.Since(tStart))
				sendKB(TGChatID, fmt.Sprintf("✅ deauthd перезапущен (PID: %s)", strings.TrimSpace(string(pid))))

			case text == "/ai":
				pendingCmd = "ai"
				logf("CMD /ai (waiting)")
				sendTG("🤖 Что хочешь спросить?")

			case strings.HasPrefix(text, "/ai "):
				prompt := strings.TrimPrefix(text, "/ai ")
				tStart := time.Now()
				logf("CMD /ai: %q", prompt)
				sendTG(fmt.Sprintf("🧠 Думаю: <i>%s</i>", prompt))
				result := aiTask(prompt)
				if len(result) > 3500 { result = result[:3500] + "\n..." }
				logf("CMD /ai: done (%s)", time.Since(tStart))
				sendKB(TGChatID, fmt.Sprintf("🤖 <b>Ответ:</b>\n%s", result))
				reqCount++
				if reqCount >= RestartEvery { reqCount = 0; go restartLLM() }

			case text == "/fix":
				pendingCmd = "fix"
				logf("CMD /fix (waiting)")
				sendTG("🔧 Что нужно починить или проверить?")

			case strings.HasPrefix(text, "/fix "):
				prompt := strings.TrimPrefix(text, "/fix ")
				tStart := time.Now()
				logf("CMD /fix: %q", prompt)
				sendTG(fmt.Sprintf("🔧 Выполняю: <i>%s</i>", prompt))
				result := aiTask(prompt)
				if len(result) > 3500 { result = result[:3500] + "\n..." }
				logf("CMD /fix: done (%s)", time.Since(tStart))
				sendKB(TGChatID, fmt.Sprintf("🔧 <b>Результат:</b>\n%s", result))
				reqCount++
				if reqCount >= RestartEvery { reqCount = 0; go restartLLM() }

			case text == "/shell":
				pendingCmd = "shell"
				logf("CMD /shell (waiting)")
				sendTG("💻 Введи команду:")

			case strings.HasPrefix(text, "/shell "):
				shellCmd := strings.TrimPrefix(text, "/shell ")
				tStart := time.Now()
				sendTG(fmt.Sprintf("<code>$ %s</code>", shellCmd))
				out, err := exec.Command("su", "-c", shellCmd).CombinedOutput()
				result := string(out)
				if err != nil { result += fmt.Sprintf("\n❌ %v", err) }
				if len(result) > 3500 { result = result[:3500] + "\n..." }
				logf("CMD /shell: %q → %d chars (%s)", shellCmd, len(result), time.Since(tStart))
				sendKB(TGChatID, fmt.Sprintf("<code>%s</code>", result))

			default:
				logf("CMD %q → shell", text)
				out, err := exec.Command("su", "-c", text).CombinedOutput()
				result := string(out)
				if err != nil { result += fmt.Sprintf("\n❌ %v", err) }
				if len(result) > 3500 { result = result[:3500] + "\n..." }
				sendKB(TGChatID, fmt.Sprintf("<code>$ %s\n%s</code>", text, result))
			}
		}
	}
}
