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
| `StatsErrorExclusionRules` | 统计采集中排除特定错误的规则（JSON） | 内置 5 条默认规则 |

---

## 统计采集

### 概述

在 relay 请求的重试循环中嵌入统计采集层，通过时间窗口聚合记录每次尝试（attempt）、整体请求（request）和异步任务执行（task execution）的统计数据。核心指标：

- 总请求数 / 成功 / 失败、总尝试数 / 成功 / 失败 / 被排除
- 重试请求数 / 重试后恢复数 / 恢复率
- TPS、平均响应时间、首字响应时间
- 异步任务提交成功率 / 执行成功率
- 渠道健康评分（0-100）

### 架构

```
接口层                       实现层                         配置层
┌───────────────────────┐   ┌────────────────────────────┐  ┌─────────────────────┐
│ RelayStatsCollector    │◄──│ MemoryStatsCollector       │  │ operation_setting/  │
│  CollectAttempt(*ptr)  │   │  WindowBuffer → RingBuffer │  │  stats_setting.go   │
│  CollectRequestComplete│   │  atomicCounters + DB persist│  │  (JSON rules in DB) │
│  CollectTaskExecution  │   └────────────────────────────┘  └──────────┬──────────┘
│  GetWindowSummaries    │                                              │ callback
│  GetTimeSeries         │   ┌────────────────────────────┐            │
│  AggregateWindows      │   │ RuleBasedClassifier        │◄───────────┘
│  GetModelStats         │   │  per-model + default groups │
└───────────────────────┘   │  AcSearch + lookup sets     │
                             └────────────────────────────┘
┌───────────────────────┐
│ ErrorClassifier        │
│  Classify() → (excl,  │
│    level, reason)      │
│  ClassifyTaskFailReason│
└───────────────────────┘
```

数据流：原始事件 → WindowBuffer（5 分钟窗口内存聚合）→ WindowSummary → RingBuffer + DB 持久化

### 文件结构

| 文件 | 内容 |
|------|------|
| `service/relay_stats.go` | 接口定义、事件结构体、维度提取器、全局注册、Noop 实现、`safeCollect`、`InitRelayStats`/`SetupStatsPersistence` |
| `service/relay_stats_classifier.go` | `ErrorExclusionRule`、`RuleBasedClassifier`（按 model 分组 + default 兜底）、JSON 解析 |
| `service/relay_stats_window.go` | `WindowBuffer`（时间窗口缓冲 + flush 循环）、`WindowSummary`、`buildSummaries` |
| `service/relay_stats_memory.go` | `MemoryStatsCollector`、`RingBuffer[T]`（泛型环形缓冲）、`atomicCounters`、时间序列构建、`windowAgg` |
| `service/relay_stats_scoring.go` | `ComputeChannelScore`（渠道健康评分公式） |
| `service/relay_stats_tracker.go` | `relayStatsTracker`（消除 Relay/RelayTask/RelayMidjourney 的重复埋点代码） |
| `service/relay_stats_persistence.go` | `StatsPersistence` 接口、GORM `dbPersistence` 实现、`stats_window_summaries` 表、定时清理 |
| `controller/relay_stats.go` | 查询 API handler（含 `parseDuration` 工具函数） |
| `controller/relay.go` | 埋点代码（`Relay()`、`RelayTask()`、`RelayMidjourney()` 中使用 `relayStatsTracker`） |
| `service/task_polling.go` | 异步任务执行完成时采集 `TaskExecutionEvent` |
| `router/api-router.go` | 路由注册 `/api/relay/stats/*` |
| `setting/operation_setting/stats_setting.go` | `StatsErrorExclusionRules` 配置项 + 内置默认规则 |
| `main.go` | 调用 `InitRelayStats()` + `SetupStatsPersistence()` + panic 采集 |

### 接口设计

#### RelayStatsCollector

```go
type RelayStatsCollector interface {
    CollectAttempt(event *AttemptEvent)              // 指针传入，分类结果回写
    CollectRequestComplete(event RequestCompleteEvent)
    CollectTaskExecution(event TaskExecutionEvent)
    GetCounters() StatsCounters                      // 全局原子计数器快照
    GetWindowSummaries(limit int) []WindowSummary    // 历史窗口 + 当前窗口 Peek
    GetTimeSeries(query TimeSeriesQuery) TimeSeriesResult
    AggregateWindows(dimensions []string) map[string]StatsCounters
    GetModelStats(startTime, endTime int64) []ModelStats  // 用户可见
    Reset()
}
```

当前实现：`MemoryStatsCollector`（WindowBuffer → RingBuffer + 原子计数器 + DB 持久化）

#### ErrorClassifier

```go
type ErrorClassifier interface {
    Classify(event AttemptEvent) (excluded bool, level int, reason string)
    ClassifyTaskFailReason(modelName string, failReason string) (excluded bool, level int, reason string)
}
```

当前实现：`RuleBasedClassifier`（按 model 分组的规则匹配，精确模型 > default 兜底）

### 事件结构体

#### AttemptEvent（单次尝试）

- `RequestID`, `AttemptIndex` — 请求标识和重试索引
- `ChannelID`, `ChannelType`, `ChannelName` — 渠道信息
- `ModelName`, `Group` — 模型和分组
- `IsAsync` — 是否为异步任务提交
- `Success` — 是否成功
- `StatusCode`, `ErrorCode`, `ErrorType`, `ErrorMessage` — 错误详情
- `ErrorLevel` — 错误严重级别（0=excluded, 1=normal, 2=serious, 3=critical）
- `Duration`, `FirstTokenDuration` — 本次尝试耗时 / 首字响应时间
- `Excluded`, `ExcludeReason` — 是否被排除规则排除（由分类器在 WindowBuffer 内设置）

#### RequestCompleteEvent（整体请求）

- `RequestID`, `UserID`, `TokenID`, `Group`, `OriginalModel`, `RelayMode` — 请求上下文
- `IsAsync` — 是否为异步任务
- `TotalAttempts` — 总尝试次数
- `FinalSuccess` — 最终是否成功
- `HasRetry` — 是否发生过重试（`TotalAttempts > 1`）
- `RetryRecovered` — 关键指标：曾失败但最终成功
- `ChannelChain` — 尝试过的渠道 ID 链（最后一个用于窗口 bucket key）
- `TotalDuration` — 总耗时
- `FirstErrorCode`, `FirstErrorStatusCode` — 首次错误信息
- `ExcludedAttempts`, `RealErrorAttempts` — 被排除 / 真实错误次数

#### TaskExecutionEvent（异步任务执行完成）

- `TaskID`, `Platform`, `ModelName`, `ChannelID`, `Group` — 任务上下文
- `Success`, `FailReason` — 执行结果（`fail_reason` 经分类器判断是否排除）
- `SubmitTime`, `FinishTime`, `ExecutionDuration` — 时间信息

### 错误排除规则

#### 规则结构

```json
{
  "model": "gpt-4",           // 可选，精确模型匹配；空/"default" 为兜底
  "channel_types": [1, 6],    // 可选，AND 前置条件
  "error_codes": ["invalid_request"],    // OR
  "status_codes": [400, 429],            // OR
  "message_keywords": ["context_length"],// OR (Aho-Corasick)
  "level": 0,                 // 0=排除, 1=普通, 2=较严重, 3=严重
  "description": "说明"
}
```

#### 匹配逻辑

1. **模型优先级**：精确匹配 model > `default` 兜底（模型组无匹配时才查默认组）
2. **规则内**：`model` + `channel_types` 是 AND 前置条件，`error_codes` / `status_codes` / `message_keywords` 是 OR
3. **规则间**：OR 关系，任一规则命中即生效
4. **关键词匹配**：复用 `service.AcSearch`（Aho-Corasick），大小写不敏感
5. **约束**：排除只影响统计计数，**不影响**重试决策、渠道禁用、错误日志等业务逻辑

#### 内置默认规则

| 规则 | 条件 | 说明 |
|------|------|------|
| 客户端参数错误 | 400/422, `invalid_request`/`bad_request_body` 等 | 非渠道故障 |
| 限流 | 429 | 暂时性 |
| 内容安全 | `sensitive_words_detected`/`prompt_blocked`, safety/blocked 关键词 | 预期行为 |
| 用户配额不足 | `insufficient_user_quota` 等 | 非渠道问题 |
| Gemini 安全拦截 | channel_type=24 + safety/blocked/recitation | Gemini 特有 |

#### 配置方式

- 通过 DB Option 表的 `StatsErrorExclusionRules` 键存储 JSON
- 运行时动态更新（通过管理 API 或 Option 更新接口）
- 定期从 DB 同步（`SyncOptions`），DB 值覆盖内置默认

### 埋点位置

#### 同步请求（`Relay()`）

通过 `relayStatsTracker` 封装，消除重复代码：

```
tracker := NewRelayStatsTracker(requestId, identity, false)

重试循环 {
    getChannel()
    attemptStart = time.Now()
    relayHandler()
    attemptDuration = since(start)

    if 成功:
        tracker.TrackAttempt(成功事件)    // 分类器在 WindowBuffer 内部调用
        tracker.Complete(true)
        return

    tracker.TrackAttempt(失败事件)        // Excluded/ErrorLevel 由分类器设置
    processChannelError()
    shouldRetry()
}

tracker.Complete(false)
```

#### 异步任务提交（`RelayTask()` / `RelayMidjourney()`）

同样使用 `relayStatsTracker`，标记 `isAsync=true`。

#### 异步任务执行完成（`task_polling.go`）

任务到达终态时调用 `SafeCollectTaskExecution()`，含执行时间和 `fail_reason` 分类。

#### Panic 捕获

在 `main.go` 的 `gin.CustomRecovery` 中采集 `ErrorLevel=3` 的失败事件。

所有采集调用通过 `safeCollect()` 包裹，panic 不影响业务。

### API 端点

**管理员权限（`RootAuth`）**，路由前缀 `/api/relay/stats`：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/` | 返回全局原子计数器快照 |
| GET | `/windows?limit=100` | 返回最近窗口摘要（含当前未 flush 窗口） |
| GET | `/timeseries?group_by=model&metric=success_rate&interval=1h&range=24h` | 时间序列（图表用） |
| GET | `/dimensions?group_by=model` | 维度聚合 |
| POST | `/reset` | 重置所有统计 |
| GET | `/exclusion_rules` | 获取当前排除规则 |
| PUT | `/exclusion_rules` | 更新排除规则（仅内存，需配合 DB 持久化） |

**用户权限（`UserAuth`）**：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/models?start_timestamp=X&end_timestamp=Y` | 用户可见的每模型统计（不暴露渠道信息） |

#### TimeSeries 参数

| 参数 | 可选值 | 默认 |
|------|--------|------|
| `group_by` | `model`, `channel`, `group` | `model` |
| `metric` | `success_rate`, `tps`, `avg_duration`, `avg_first_token`, `channel_score`, `request_success_rate`, `task_exec_success_rate` | `success_rate` |
| `interval` | `5m`, `1h`, `6h` | `5m` |
| `range` | `1h`, `6h`, `24h`, `7d` | `24h` |

### 维度聚合

采用 **查询时聚合（compute-on-read）** 方案：

- 写入路径：原始事件 → WindowBuffer（按 model+channel+group 分 bucket）→ 窗口摘要 → RingBuffer + DB
- 查询路径：从 RingBuffer 快照 + 当前窗口 Peek 中按请求维度实时聚合
- 新增维度：`RegisterWindowDimension("name", extractorFunc)`

内置维度：

| 维度 | 提取方式 |
|------|---------|
| `model` | `WindowSummary.ModelName` |
| `channel` | `WindowSummary.ChannelID`（0 过滤） |
| `group` | `WindowSummary.Group` |

多维度可组合：`group_by=model,channel` → 复合键 `"gpt-4:1"`

### 数据持久化

| 组件 | 存储 | 恢复 |
|------|------|------|
| 窗口摘要 | 每次 flush 写入 `stats_window_summaries` 表 | 启动时从 DB 加载最近 7 天 |
| 排除规则 | DB Option 表 `StatsErrorExclusionRules` | 启动时加载 |
| 全局计数器 | 仅内存（原子操作） | 不持久化，重启归零 |
| 旧数据清理 | 每小时检查，删除 7 天前的行 | — |

### 设计要点

- **不影响业务**：采集失败静默忽略，`noopCollector` 默认零开销
- **排除只影响统计**：不影响 `shouldRetry`、`ShouldDisableChannel`、`processChannelError`
- **时间窗口聚合**：5 分钟窗口，RingBuffer 10000 条 ≈ 覆盖 34 天
- **实时查询**：`Peek()` 包含当前未 flush 窗口数据，TPS 最少用 30s 窗口避免尖刺
- **线程安全**：计数器用 `atomic`，窗口缓冲/环形缓冲/维度提取器各有独立 `sync.Mutex`/`sync.RWMutex`
- **接口扩展**：实现 `RelayStatsCollector` 接口即可替换存储后端
- **动态配置**：排除规则通过 DB 持久化，支持运行时更新，内置 5 条默认规则开箱即用
