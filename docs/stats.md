Request success or fail stats

## Request error handling

Request error handling is in `controller/relay.go`

### 错误类型 (`types/error.go`)

核心结构体 `NewAPIError`：

| 字段            | 类型             | 说明                                           |
| --------------- | ---------------- | ---------------------------------------------- |
| `Err`           | `error`          | 原始错误                                       |
| `RelayError`    | `any`            | 上游返回的原始错误体（OpenAI / Claude 格式）    |
| `StatusCode`    | `int`            | HTTP 状态码                                    |
| `skipRetry`     | `bool`           | 是否跳过重试                                   |
| `recordErrorLog`| `*bool`          | 是否记录错误日志（nil 视为 true）               |
| `errorType`     | `ErrorType`      | 错误分类（upstream / local 等）                 |
| `errorCode`     | `ErrorCode`      | 错误码，`channel:` 前缀标识渠道级错误           |

错误选项（functional options 模式）：

- `ErrOptionWithSkipRetry()` — 标记此错误不应触发重试
- `ErrOptionWithNoRecordErrorLog()` — 不记录错误日志
- `ErrOptionWithHideErrMsg(msg)` — 替换错误信息，隐藏敏感上游细节

### Relay 主流程 (`Relay` 函数)

```
请求进入 → 校验/解析 → 预扣费 → 重试循环 → 响应/退款
```

#### 1. 请求校验阶段（失败即返回，不进入重试）

| 校验步骤 | 错误码 | 是否重试 |
|---------|--------|---------|
| 请求体解析 (`GetAndValidateRequest`) | `ErrorCodeInvalidRequest` / `ErrorCodeReadRequestBodyFailed`(413) | 否 |
| 生成 RelayInfo (`GenRelayInfo`) | `ErrorCodeGenRelayInfoFailed` | 否 |
| 敏感词检测 (`CheckSensitiveText`) | `ErrorCodeSensitiveWordsDetected` | 否 |
| 预估 Token (`EstimateRequestToken`) | `ErrorCodeCountTokenFailed` | 否 |
| 模型价格 (`ModelPriceHelper`) | `ErrorCodeModelPriceError` | 否 |
| 预扣费 (`PreConsumeBilling`) | 由计费服务决定 | 否 |

#### 2. 重试循环

```go
for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
    1. getChannel()       — 选择可用渠道（失败则 break）
    2. GetBodyStorage()   — 恢复请求体（失败则 break，413 不重试）
    3. relayHandler()     — 转发到上游
    4. 成功 → return
    5. 失败 → processChannelError() + shouldRetry() 决策
}
```

- **渠道选择** (`service.CacheGetRandomSatisfiedChannel`)：按分组 + 模型名查找可用渠道，`retry` 参数作为优先级索引（retry=0 取最高优先级，retry=1 取次优先级，以此类推）
- **Auto 分组跨组重试**：当 `TokenGroup == "auto"` 时，按 `autoGroups` 顺序遍历分组，每个分组内用完优先级后切换到下一个分组，并通过 `ResetRetryNextTry()` 重置计数器
- **请求体恢复**：每次重试前通过 `GetBodyStorage()` + `io.NopCloser` 重建 `c.Request.Body`，使同一请求体可被多次读取

#### 3. 重试决策 (`shouldRetry`)

按以下优先级依次判断：

| 优先级 | 条件 | 结果 |
|-------|------|------|
| 1 | 渠道亲和性失败 (`ShouldSkipRetryAfterChannelAffinityFailure`) | **不重试** |
| 2 | Channel 错误 (`errorCode` 以 `channel:` 开头) | **重试** |
| 3 | `skipRetry == true` | **不重试** |
| 4 | 剩余重试次数 ≤ 0 | **不重试** |
| 5 | 指定了特定渠道 (`specific_channel_id`) | **不重试** |
| 6 | HTTP 2xx | **不重试** |
| 7 | HTTP 状态码不在 100–599 范围 | **重试** |
| 8 | `ErrorCode` 在永不重试列表中（如 `bad_response_body`） | **不重试** |
| 9 | 按状态码范围匹配 (`ShouldRetryByStatusCode`) | 匹配则重试 |

**可重试的状态码范围**（`AutomaticRetryStatusCodeRanges`）：

- 1xx, 3xx, 401–407, 409–499, 500–503, 505–523, 525–599

**永不重试的状态码**：504 (Gateway Timeout), 524 (Cloudflare Timeout)

#### 4. Violation Fee 归一化

在重试决策 **之前** 调用 `NormalizeViolationFeeError()`：

- 如果检测到 CSAM violation 标记 → 包装为 violation-fee 错误 + 设置 skipRetry
- 如果错误码已包含 violation-fee 前缀 → 设置 skipRetry

确保违规计费的错误不会因重试而产生重复扣费。

### 渠道错误处理 (`processChannelError`)

每次重试失败都会触发：

#### 1. 自动禁用渠道 (`ShouldDisableChannel`)

当 `AutomaticDisableChannelEnabled` 且渠道设置了 `AutoBan` 时，满足以下任一条件会**异步**禁用渠道：

| 条件类型 | 示例 |
|---------|------|
| Channel 错误码 | `channel:` 前缀的 ErrorCode |
| 状态码匹配 | 由 `ShouldDisableByStatusCode` 判断 |
| Gemini 403 | Gemini 渠道返回 403 Forbidden |
| 特定 OpenAI 错误码 | `invalid_api_key`, `account_deactivated`, `billing_not_active`, `Arrearage` |
| 特定错误类型 | `insufficient_quota`, `authentication_error`, `permission_error`, `forbidden` |
| 关键词匹配 | 错误信息包含 `AutomaticDisableKeywords` 配置的关键词（AC 自动机匹配） |

#### 2. 错误日志记录

当 `ErrorLogEnabled` 且错误标记为需要记录（`IsRecordErrorLog`）时，记录到数据库：

- 用户 ID、Token ID/Name、模型名、分组
- 错误类型/错误码/状态码
- 渠道信息（ID、名称、类型、是否多 Key）
- 请求路径、使用的渠道链路 (`use_channel`)
- 渠道亲和性信息
- 请求耗时

### 退款机制

通过 `defer` 确保在请求失败时退款：

```go
defer func() {
    if newAPIError != nil {
        newAPIError = service.NormalizeViolationFeeError(newAPIError)
        if relayInfo.Billing != nil {
            relayInfo.Billing.Refund(c)   // 退还预扣的配额
        }
        service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)  // 违规扣费
    }
}()
```

- 预扣费在重试循环前完成，所有重试共用一次预扣费
- 重试循环结束后仍失败 → 退款 + 检查是否需要违规扣费
- 重试成功 → `newAPIError == nil`，不触发退款

### Task Relay 重试 (`RelayTask`)

Task 类型请求（Midjourney 等异步任务）有独立的重试逻辑 `shouldRetryTaskRelay`，与标准 Relay 的差异：

| 特性 | 标准 Relay | Task Relay |
|------|-----------|------------|
| 重试决策函数 | `shouldRetry` | `shouldRetryTaskRelay` |
| Channel 错误强制重试 | 是 | 否 |
| skipRetry 标记 | 支持 | 不支持（由 `LocalError` 判断） |
| 429 Too Many Requests | 按状态码范围匹配 | 强制重试 |
| 307 Temporary Redirect | 按状态码范围匹配 | 强制重试 |
| 400 Bad Request | 按状态码范围匹配（会重试） | **不重试** |
| 408 Request Timeout | 按状态码范围匹配（会重试） | **不重试**（Azure 超时） |
| 锁定渠道 (LockedChannel) | 不支持 | 支持，锁定渠道时每次重试重新 Setup |

### 错误响应格式

根据 `relayFormat` 返回不同格式的错误：

| 格式 | 响应方式 |
|------|---------|
| OpenAI (默认) | `{"error": {type, message, code, param}}` |
| Claude | `{"type": "error", "error": {type, message}}` |
| OpenAI Realtime (WebSocket) | 通过 WebSocket 发送错误帧 |
| Task | `TaskError` 结构体（含 429 限流提示改写） |

### 配置项

| 配置 | 说明 | 默认值 |
|------|------|-------|
| `RetryTimes` | 最大重试次数 | 0（不重试） |
| `AutomaticDisableChannelEnabled` | 是否启用自动禁用渠道 | - |
| `ErrorLogEnabled` | 是否记录错误日志到数据库 | - |
| `AutomaticDisableKeywords` | 自动禁用渠道的关键词列表 | - |
| `AutomaticRetryStatusCodeRanges` | 可重试的 HTTP 状态码范围 | 见上文 |
| `StatsErrorExclusionRules` | 统计采集中排除特定错误的规则（JSON） | `[]` |

---

## 统计采集

### 概述

在 relay 请求的重试循环中嵌入统计采集层，记录每次尝试（attempt）和整体请求（request）的成功/失败结果。关键统计维度：

- 总请求数 / 成功 / 失败
- 总尝试数 / 成功 / 失败 / 被排除
- 重试请求数 / 重试后恢复数 / 恢复率

### 架构

```
接口层                    实现层                    配置层
┌──────────────────┐    ┌──────────────────────┐  ┌──────────────────────┐
│ RelayStatsCollector│◄───│ MemoryStatsCollector │  │ operation_setting/   │
│   CollectAttempt  │    │  RingBuffer + Atomic │  │  stats_setting.go    │
│   CollectComplete │    └──────────────────────┘  │  (JSON rules in DB)  │
└──────────────────┘                               └──────────┬───────────┘
                                                              │ callback
┌──────────────────┐    ┌──────────────────────┐              │
│ ErrorClassifier   │◄───│ RuleBasedClassifier  │◄─────────────┘
│   Classify        │    │  AcSearch + lookup   │
└──────────────────┘    └──────────────────────┘
```

### 文件结构

| 文件 | 内容 |
|------|------|
| `service/relay_stats.go` | 接口定义（`RelayStatsCollector`, `ErrorClassifier`）、事件结构体、全局注册、Noop 实现、`safeCollect`、`InitRelayStats` |
| `service/relay_stats_classifier.go` | `ErrorExclusionRule` 结构体、`RuleBasedClassifier` 实现、JSON 解析 |
| `service/relay_stats_memory.go` | `MemoryStatsCollector`、`RingBuffer[T]`（泛型环形缓冲）、`atomicCounters` |
| `controller/relay_stats.go` | 查询 API handler |
| `controller/relay.go` | 埋点代码（`Relay()` 和 `RelayTask()` 重试循环中） |
| `router/api-router.go` | 路由注册 `/api/relay/stats/*` |
| `setting/operation_setting/stats_setting.go` | `StatsErrorExclusionRules` 配置项管理 |
| `main.go` | 调用 `service.InitRelayStats()` 初始化 |

### 接口设计

#### RelayStatsCollector

```go
type RelayStatsCollector interface {
    CollectAttempt(event AttemptEvent)
    CollectRequestComplete(event RequestCompleteEvent)
    GetCounters() StatsCounters
    GetRecentAttempts(limit int) []AttemptEvent
    GetRecentRequests(limit int) []RequestCompleteEvent
    Reset()
}
```

当前实现：`MemoryStatsCollector`（内存环形缓冲 + 原子计数器）
后续可替换为 Redis / DB / Prometheus 等持久化方案，只需实现此接口。

#### ErrorClassifier

```go
type ErrorClassifier interface {
    Classify(event AttemptEvent) (excluded bool, reason string)
}
```

当前实现：`RuleBasedClassifier`（基于可配置的排除规则）

### 事件结构体

#### AttemptEvent（单次尝试）

- `RequestID`, `AttemptIndex` — 请求标识和重试索引
- `ChannelID`, `ChannelType`, `ChannelName` — 渠道信息
- `ModelName`, `Group` — 模型和分组
- `Success` — 是否成功
- `StatusCode`, `ErrorCode`, `ErrorType`, `ErrorMessage` — 错误详情
- `Duration` — 本次尝试耗时
- `Excluded`, `ExcludeReason` — 是否被排除规则排除

#### RequestCompleteEvent（整体请求）

- `RequestID`, `UserID`, `TokenID`, `Group`, `OriginalModel`, `RelayMode` — 请求上下文
- `TotalAttempts` — 总尝试次数
- `FinalSuccess` — 最终是否成功
- `HasRetry` — 是否发生过重试（`TotalAttempts > 1`）
- `RetryRecovered` — 关键指标：曾失败但最终成功
- `ChannelChain` — 尝试过的渠道 ID 链
- `TotalDuration` — 总耗时
- `FirstErrorCode`, `FirstErrorStatusCode` — 首次错误信息
- `ExcludedAttempts`, `RealErrorAttempts` — 被排除 / 真实错误次数

### 错误排除规则

#### 规则结构

```json
[
  {
    "description": "Client parameter errors",
    "error_codes": ["invalid_request_error", "invalid_parameter"],
    "status_codes": [400]
  },
  {
    "channel_types": [1, 6],
    "message_keywords": ["context_length_exceeded", "maximum context length"],
    "description": "OpenAI context length exceeded"
  },
  {
    "status_codes": [429],
    "description": "Rate limiting is transient"
  },
  {
    "channel_types": [24],
    "error_codes": ["prompt_blocked"],
    "message_keywords": ["safety", "blocked"],
    "description": "Gemini safety block"
  }
]
```

#### 匹配逻辑

- 规则内：`channel_types` 是 AND 条件（必须匹配），`error_codes` / `status_codes` / `message_keywords` 是 OR 条件（任一匹配即可）
- 规则间：OR 关系，任一规则匹配则排除
- 关键词匹配复用 `service.AcSearch`（Aho-Corasick 多模式匹配），大小写不敏感
- 排除只影响统计计数，**不影响**重试决策、渠道禁用、错误日志等业务逻辑

#### 配置方式

通过 DB Option 表的 `StatsErrorExclusionRules` 键存储 JSON，支持：
- 运行时动态更新（通过管理 API 或 Option 更新接口）
- 定期从 DB 同步（`SyncOptions`）

### 埋点位置

在 `controller/relay.go` 的 `Relay()` 和 `RelayTask()` 重试循环中：

```
重试循环 {
    getChannel()
    attemptStart = time.Now()       ← 新增
    relayHandler()
    attemptDuration = since(start)  ← 新增

    if 成功:
        CollectAttempt(成功事件)      ← 新增
        CollectRequestComplete(...)  ← 新增
        return

    CollectAttempt(失败事件)          ← 新增（classifier 在 collector 内部调用）
    processChannelError()
    shouldRetry()
}

CollectRequestComplete(失败事件)      ← 新增（循环外，最终失败）
```

所有采集调用通过 `safeCollect()` 包裹，panic 不影响业务。

### API 端点

管理员权限（`RootAuth`），路由前缀 `/api/relay/stats`：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/` | 返回聚合计数器 |
| GET | `/recent?type=attempts&limit=100` | 返回最近事件明细（`type=attempts` 或 `requests`） |
| POST | `/reset` | 重置所有统计 |
| GET | `/exclusion_rules` | 获取当前排除规则 |
| PUT | `/exclusion_rules` | 更新排除规则 |

聚合响应示例：

```json
{
  "success": true,
  "data": {
    "total_requests": 15000,
    "success_requests": 14500,
    "failed_requests": 500,
    "total_attempts": 16200,
    "success_attempts": 15100,
    "failed_attempts": 900,
    "excluded_attempts": 200,
    "retry_requests": 800,
    "retry_recovered": 600,
    "retry_recovery_rate": 0.75
  }
}
```

### 维度聚合

#### 设计方案

采用 **事件驱动、查询时聚合（compute-on-read）** 方案：

- 写入路径只做一件事：把事件推入 `RingBuffer`，同时更新全局 `atomicCounters`
- 查询路径从 `RingBuffer` 快照中按请求维度实时聚合，无需预计算
- 新增维度只需注册一个 extractor 函数，历史事件（缓冲区内）可立即回溯

#### 支持的维度

| 维度 | 事件类型 | 提取方式 |
|------|---------|---------|
| `model` | attempts / requests | `AttemptEvent.ModelName` / `RequestCompleteEvent.OriginalModel` |
| `channel` | attempts | `AttemptEvent.ChannelID`（数字转字符串） |
| `channel_type` | attempts | `AttemptEvent.ChannelType` |
| `group` | attempts / requests | `Group` 字段 |

多个维度可组合使用，例如 `group_by=model,channel` 会生成 `"gpt-4:1"` 形式的复合键。

#### 扩展自定义维度

```go
// 注册新的 attempt 维度
service.RegisterAttemptDimension("region", func(e service.AttemptEvent) string {
    return e.Region // 需先在 AttemptEvent 中添加该字段
})

// 注册新的 request 维度
service.RegisterRequestDimension("user", func(e service.RequestCompleteEvent) string {
    return strconv.Itoa(e.UserID)
})
```

#### API

```
GET /api/relay/stats/dimensions?group_by=model&type=attempts
GET /api/relay/stats/dimensions?group_by=model,channel&type=attempts
GET /api/relay/stats/dimensions?group_by=model&type=requests
```

**参数：**

- `group_by`：逗号分隔的维度名（`model`, `channel`, `channel_type`, `group`）
- `type`：`attempts`（默认）或 `requests`

**响应示例：**

```json
{
  "success": true,
  "data": {
    "gpt-4": {
      "total_attempts": 120,
      "success_attempts": 100,
      "failed_attempts": 15,
      "excluded_attempts": 5
    },
    "claude-3": {
      "total_attempts": 80,
      "success_attempts": 75,
      "failed_attempts": 5,
      "excluded_attempts": 0
    }
  },
  "group_by": ["model"],
  "event_type": "attempts"
}
```

### 设计要点

- **不影响业务**：采集失败静默忽略，`NoopCollector` 默认零开销
- **排除只影响统计**：排除规则不影响 `shouldRetry`、`ShouldDisableChannel`、`processChannelError` 等
- **接口扩展**：后续实现 `RedisStatsCollector` / `DBStatsCollector` 只需实现 `RelayStatsCollector` 接口
- **线程安全**：计数器用 `atomic`，环形缓冲用 `sync.RWMutex`
- **内存可控**：环形缓冲固定大小（默认 10000），写满覆盖最旧条目
- **动态配置**：排除规则通过 DB 持久化，支持运行时更新，无需重启
- **维度灵活**：compute-on-read 方案，新增维度无需改写入逻辑，缓冲区内历史事件可立即按新维度回溯
