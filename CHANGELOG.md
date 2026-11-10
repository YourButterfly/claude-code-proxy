# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **长流被 300s 整体超时腰斩**（`stream read error: context deadline exceeded`）
  - 根因：流式上游用 `http.Client{Timeout: 300s}`。该字段覆盖整个交换过程
    **包括响应体读取**，所以在流式接口上它限制的是"生成能持续多久"，而不是
    "故障多久被发现"。健康的长流也会被拦腰切断。
  - 实测：每 2s 吐一块、持续 340s 的上游，在 **恰好 300.0s** 被切断，
    `quiet_for=0.8s`（连接完全正常）；发完头就静默的上游同样 300.0s。
    两者在客户端表现完全相同，无法区分。
  - 现场：run 268 有三次调用结束于 300.4–300.5s、token 全为 0；而成功的
    生成调用分别耗时 345s、395s、552s、564s。整体超时设多高都不安全。
  - 修复：上游客户端不再设整体 `Timeout`，改为分别约束两种真实故障：
    - `streamingResponseHeaderTimeout`（120s，Transport 层）— 上游不回头部
    - `streamIdleTimeout`（默认 180s，可用 `STREAM_IDLE_TIMEOUT` 覆盖）—
      上游回了头但中途静默；每次成功读取都重置计时，长流不受影响
  - 同时补上：下游客户端断开后停止继续读取上游（此前这个泄漏被 300s
    整体超时掩盖，去掉后必须显式处理，否则每个废弃请求都会一直拉起上游）

### Added
- 流式诊断日志（不受 `-d`/`-s` 开关影响，生产默认开启）：
  `[STREAM-OPEN]` / `[STREAM-ERROR]` / `[STREAM-CLOSE]` /
  `[UPSTREAM-ERROR]` / `[UPSTREAM-STATUS]`
  - 每条带 `tag=`，可把同一请求的 OPEN/ERROR/CLOSE 对上，多路并发不混淆
  - `bytes_from_upstream` / `events` / `first_chunk_after` / `quiet_for`
    直接回答"上游到底有没有发过数据、沉默多久、断在哪一侧"
  - 此前代理以无 flag 启动（`proxy_manager` 不带 `-s`/`-d`），
    一次请求日志都没有，故障只能看到客户端一句 "stream read error"

## [1.3.0] - 2025-11-13

### Added
- **Adaptive Per-Model Capability Detection** - Complete refactor replacing hardcoded patterns (#7)
  - Automatically learns which parameters each `(provider, model)` combination supports
  - Per-model capability caching with `CacheKey{BaseURL, Model}` structure
  - Thread-safe in-memory cache protected by `sync.RWMutex`
  - Debug logging for cache hits/misses visible with `-d` flag
- **Zero-Configuration Provider Compatibility**
  - Works with any OpenAI-compatible provider without code changes
  - Automatic retry mechanism with error-based detection
  - Broad keyword matching for parameter error detection
  - No status code restrictions (handles misconfigured providers)
- **OpenWebUI Support** - Native support for OpenWebUI/LiteLLM backends
  - Automatically adapts to OpenWebUI's parameter quirks
  - First request detection (~1-2s penalty), instant subsequent requests
  - Tested with GPT-5 and GPT-4.1 models

### Changed
- **Removed ~100 lines of hardcoded model patterns**
  - Deleted `IsReasoningModel()` function with gpt-5/o1/o2/o3/o4 patterns
  - Deleted `FetchReasoningModels()` function and OpenRouter API calls
  - Deleted `ReasoningModelCache` struct and related code
  - Removed unused imports: `encoding/json`, `net/http` from config.go
- **Refactored capability detection system**
  - Changed from per-provider to per-model caching
  - Struct-based cache keys (zero collision risk vs string concatenation)
  - `GetProviderCapabilities()` → `GetModelCapabilities()`
  - `SetProviderCapabilities()` → `SetModelCapabilities()`
  - `ShouldUseMaxCompletionTokens()` now uses per-model cache
- **Enhanced retry logic in handlers.go**
  - `isMaxTokensParameterError()` uses broad keyword matching
  - `retryWithoutMaxCompletionTokens()` caches per-model capabilities
  - Applied to both streaming and non-streaming handlers
  - Removed status code restrictions for better provider compatibility

### Removed
- Hardcoded reasoning model patterns (gpt-5*, o1*, o2*, o3*, o4*)
- OpenRouter reasoning models API integration
- Provider-specific hardcoding for Unknown provider type
- Unused configuration imports and dead code

### Technical Details
- **Cache Structure**: `map[CacheKey]*ModelCapabilities` where `CacheKey{BaseURL, Model}`
- **Detection Flow**: Try max_completion_tokens → Error → Retry → Cache result
- **Error Detection**: Broad keyword matching (parameter + unsupported/invalid) + our param names
- **Cache Scope**: In-memory, thread-safe, cleared on restart
- **Benefits**: Future-proof, zero user config, ~70 net lines removed

### Documentation
- Added "Adaptive Per-Model Detection" section to README.md with full implementation details
- Updated CLAUDE.md with comprehensive per-model caching documentation
- Cleaned up docs/ folder - removed planning artifacts and superseded documentation

### Philosophy
This release embodies the project philosophy: "Support all provider quirks automatically - never burden users with configurations they don't understand." The adaptive system eliminates special-casing and works with any current or future OpenAI-compatible provider.

## [1.2.0] - 2025-11-01

### Added
- **Complete CHANGELOG.md** following Keep a Changelog format
  - Full history for v1.0.0, v1.1.0, and v1.2.0
  - Categorized changes (Added, Changed, Fixed, etc.)
  - Upgrade guides and release notes
- **Release documentation system**
  - `.github/RELEASE_TEMPLATE.md` with step-by-step checklist
  - `.github/RELEASE_WORKFLOW.md` with complete workflow guide
  - Conventional commits guidelines
  - Semantic versioning strategy
- **Professional README badges**
  - Version badge (links to latest release)
  - Go version badge
  - Build status badge
  - License badge
  - Open issues badge
- **Comprehensive unit tests** for dynamic reasoning model detection
  - 36 new test cases covering hardcoded fallback patterns
  - OpenRouter API cache behavior tests
  - Provider-specific detection tests
  - Edge cases and error handling tests
  - Mock HTTP server tests for OpenRouter API integration

### Changed
- **Automated release workflow** now extracts release notes from CHANGELOG.md
  - Falls back to auto-generated notes if no changelog section found
  - Cleaner workflow logic with proper error handling
- Updated reasoning model detection tests to use `cfg.IsReasoningModel()` method

### Fixed
- Linter error for unchecked `resp.Body.Close()` in `FetchReasoningModels()`

## [1.1.0] - 2025-10-31

### Added
- **Dynamic reasoning model detection** from OpenRouter API (#5)
  - Automatically fetches list of reasoning-capable models on startup
  - Caches 130+ reasoning models (DeepSeek-R1, Gemini, GPT-5, o-series, etc.)
  - Falls back to hardcoded pattern matching for OpenAI Direct and Ollama
- Robust `max_completion_tokens` parameter detection for reasoning models
- Provider-specific model detection (OpenRouter uses API cache, others use patterns)

### Changed
- Moved reasoning model detection from standalone function to `Config` method
- Improved model detection to support dynamic discovery of new reasoning models
- Enhanced `IsReasoningModel()` to check provider type before using cache

### Technical Details
- Uses OpenRouter's `supported_parameters=reasoning` endpoint (no auth required)
- Asynchronous model fetching to avoid blocking startup
- Global `ReasoningModelCache` with populated flag for fallback behavior

## [1.0.0] - 2025-10-26

### Added
- **Initial release** of Claude Code Proxy
- Bidirectional API format conversion (Claude ↔ OpenAI)
- **Multi-provider support**:
  - OpenRouter (200+ models including Grok, Gemini, DeepSeek)
  - OpenAI Direct (GPT-4, GPT-5, o1, o3)
  - Ollama (local models)
- **Full Claude Code feature compatibility**:
  - Tool calling (function calling)
  - Extended thinking blocks (from reasoning models)
  - Streaming responses with SSE
  - Token usage tracking
- **Pattern-based model routing**:
  - Haiku → lightweight models (gpt-5-mini, gemini-flash)
  - Sonnet → flagship models (gpt-5, grok)
  - Opus → premium models (gpt-5, o3)
- **Daemon mode** with background process management
- **`ccp` wrapper script** for seamless Claude Code integration
- **Simple log mode** (`-s` flag) with one-line request summaries and throughput metrics
- **Debug mode** (`-d` flag) for full request/response logging
- Environment variable configuration via `.env` files
- Provider-specific parameter injection:
  - OpenRouter: `reasoning: {enabled: true}`, `usage: {include: true}`
  - OpenAI Direct: `reasoning_effort: "medium"` for GPT-5
  - Ollama: `tool_choice: "required"` when tools present
- Comprehensive unit tests for converter and tool calling

### Fixed
- Thinking blocks now use correct `thinking` field (not `text`)
- Streaming token usage continues past `finish_reason`
- Tool calling format conversion between Claude and OpenAI
- Encrypted reasoning blocks from models like Grok (skipped, not shown)
- HTTP logging now respects simple log mode setting
- Golangci-lint errors for CI/CD pipeline

### Changed
- Replaced hardcoded model names with constants
- Removed unused configuration options (`MAX_TOKENS_LIMIT`, `REQUEST_TIMEOUT`)
- Simplified Sonnet pattern to match all versions (sonnet-3, sonnet-4, sonnet-5)
- Updated documentation to remove o1/o3 references from defaults

### Documentation
- Complete CLI command and flag documentation
- Environment variable override documentation
- CLAUDE.md for AI-assisted development
- Beta software disclaimer
- MIT License

### Infrastructure
- GitHub Actions workflows for CI/CD
- golangci-lint integration
- Automated testing pipeline
- Claude Code Review workflow
- Claude PR Assistant workflow

## [0.1.0] - Initial Development

### Added
- Manual Anthropic-to-OpenAI proxy implementation (proof of concept)

---

## Release Notes

### How to Use This Changelog

- **Unreleased**: Changes in `main` branch not yet released
- **[X.Y.Z]**: Released versions with dates
- **Categories**:
  - `Added`: New features
  - `Changed`: Changes to existing functionality
  - `Deprecated`: Soon-to-be removed features
  - `Removed`: Removed features
  - `Fixed`: Bug fixes
  - `Security`: Security fixes

### Upgrade Guide

#### From v1.0.0 to v1.1.0
- No breaking changes
- Dynamic reasoning model detection happens automatically
- Existing `.env` configurations remain compatible
- Hardcoded fallback ensures compatibility if OpenRouter API fetch fails

---

**Links:**
- [v1.1.0 Release](https://github.com/nielspeter/claude-code-proxy/releases/tag/v1.1.0)
- [v1.0.0 Release](https://github.com/nielspeter/claude-code-proxy/releases/tag/v1.0.0)
