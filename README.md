# ai-node — On-device LLM inference on Android GPU

Qwen2.5 1.5B running on Adreno 660 via Vulkan (Mesa Turnip). Fully local — no internet, no API keys, no cloud required.

```
Telegram → bridge.go → llama.cpp → Vulkan (Turnip) → Adreno 660
```

## Performance

| Model | Size | Prompt | Generation |
|-------|------|--------|-----------|
| Qwen2.5 1.5B Q8_0 | 1.8 GB | 24 t/s | 8 t/s |
| Qwen2.5 0.5B Q8_0 | 0.7 GB | 54 t/s | 8 t/s |

Device: Realme GT (Snapdragon 888+, 2021), GPU: Adreno 660.

## How it works

1. Message arrives from Telegram
2. Bridge collects system data (temperature, battery, memory, disk)
3. Prompt sent to llama.cpp via HTTP
4. llama.cpp runs inference on GPU via Vulkan (Mesa Turnip)
5. Response returned to Telegram

## Known issue: GPU state corruption

Mesa Turnip (open-source Vulkan driver for Adreno) has a bug — after ~50 inference requests, GPU state corrupts and output becomes `@@@@@`.

**Fixed by:**
- Each response is checked for `@@@@@` pattern
- If garbage detected — server silently restarts, request retried
- Proactive restart every 10 requests reduces failure probability
- `--cache-ram 0` and `--no-kv-offload` disable caching mechanisms that trigger the bug

Everything is transparent — user only sees correct responses.

## System data injection

Before each inference, bridge runs 4 shell commands:

```
cat /sys/class/thermal/thermal_zone*/temp  → CPU/GPU temperature
dumpsys battery                              → charge level and temperature
free -m                                      → memory
df -h /data                                  → disk usage
```

Results are injected into the system prompt. The model can answer "CPU is 45°C, battery is 37°C" with real numbers.

## Telegram commands

- `/ai question` — ask the assistant
- `/fix issue` — debugging assistance
- `/shell command` — execute shell command on device
- `/status` — system health
- `/log` — bridge logs
- `/restart_deauthd` — restart network daemon

## Stack

| Component | Technology |
|-----------|-----------|
| LLM | llama.cpp (NDK build, Vulkan backend) |
| GPU driver | Mesa Turnip 26.2 (Vulkan 1.4) |
| Model | Qwen2.5 1.5B / 0.5B Q8_0 |
| Bridge | Go 1.22 |
| OS | Android 13, Magisk root |
| Autorun | Magisk service.d |

## File layout

```
/data/local/tmp/
├── llama-vk/
│   ├── llama-server        # Main server
│   ├── llama-cli          # CLI for testing
│   └── *.so               # Shared libraries (ggml-vulkan, etc)
├── qwen_15.gguf            # 1.5B model (~1.8 GB)
├── qwen05.gguf             # 0.5B model (optional)
├── bridge                  # Go Telegram bot
├── deauthd                 # Network daemon (optional)
└── llm_servers.sh          # Boot script
```

## Building

```bash
# Requires: Android NDK
cmake -DCMAKE_TOOLCHAIN_FILE=$NDK/build/cmake/android.toolchain.cmake \
  -DANDROID_ABI=arm64-v8a -DANDROID_PLATFORM=android-30 \
  -DGGML_VULKAN=ON -DGGML_OPENMP=OFF \
  /path/to/llama.cpp
cmake --build . --target llama-server --target llama-cli -j8
```

## Local build

```bash
cp config.go.example config.go
# edit config.go with your tokens
go build -o bridge bridge.go config.go
```
