# Usage 构造详细逻辑

本文档描述 Kiro-Go 在向客户端返回响应时，如何构造 `usage`（token 计数）字段。涵盖四条响应路径：Claude 流式、Claude 非流式、OpenAI 流式/非流式、OpenAI Responses API。

---

## 一、总体流水线

每条请求路径最终都经历以下五个阶段：

```
① 预估输入 token（本地估算，作为兜底）
       ↓
② 上游流式解析（EventStream）→ 收集 inputTokens、outputTokens、credits、contextUsagePercentage
       ↓
③ 确定最终 inputTokens（优先级：contextUsage > 上游 token 字段 > 本地预估）
④ 确定最终 outputTokens（Claude：本地重新估算；OpenAI：本地重新估算）
       ↓
⑤ credit 校准（仅 Claude 路径）：对 outputTokens 和 cache 组件做比例缩放
       ↓
⑥ 填写响应体中的 usage 字段
```

---

## 二、阶段详解

### 阶段①：预估输入 token（`estimatedInputTokens`）

在请求发出前，由 `proxy/token_estimator.go` 本地估算：

- **Claude 请求**：`estimateClaudeRequestInputTokens(req)` — 遍历 system、messages、tools，对每段文本调用 `estimateApproxTokens()`。
- **OpenAI 请求**：`estimateOpenAIRequestInputTokens(req)` — 遍历 messages（含 tool_call、tool_result）和 tools。

`estimateApproxTokens(text string) int` 的计算公式：

```
estimated = ceil(
    regularASCII / 4.5 +
    digits       / 2.0 +
    symbols      / 1.5 +
    nonASCII     / 1.5
)
```

其中 nonASCII 按每 1.5 个 rune 约 1 个 token 估算（适用于中文等多字节字符）。

---

### 阶段②：上游 EventStream 解析（`kiro.go`）

`CallKiroAPI` 解析 AWS EventStream 二进制协议，从中提取三类数值：

#### 2a. inputTokens / outputTokens（来自 `updateTokensFromEvent`）

对每个 event 以及其 `usage` / `tokenUsage` 子字段，按以下优先级读取：

| 优先级 | 字段名（camelCase 或 snake_case 均支持） | 说明 |
|--------|------------------------------------------|------|
| 1      | `outputTokens` / `completionTokens` / `totalOutputTokens` | output 直接字段 |
| 2      | `inputTokens` / `promptTokens` / `totalInputTokens` | input 直接字段 |
| 3      | `uncachedInputTokens + cacheReadInputTokens + cacheWriteInputTokens` | 各缓存组件之和 |
| 4      | `totalTokens - outputTokens` | 若只有总 token |

最后一次出现的值覆盖前值（以流中最后一个 token event 为准）。

#### 2b. credits（来自 `meteringEvent`）

```go
case "meteringEvent":
    if usage, ok := event["usage"].(float64); ok {
        totalCredits += usage
    }
```

将流中所有 `meteringEvent.usage` 累加为 `totalCredits`。

#### 2c. contextUsagePercentage（来自 `contextUsageEvent`）

```go
case "contextUsageEvent":
    realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
```

`getContextWindowSize(model)` 按模型版本返回：
- Claude ≥ 4.6（sonnet-4.6, opus-4.6, opus-4.7, opus-4.8…）→ **1,000,000**
- Claude ≤ 4.5（opus-4.5, sonnet-4.5, haiku-4.5 等）→ **200,000**

---

### 阶段③：确定最终 inputTokens

```go
if realInputTokens > 0 {
    inputTokens = realInputTokens      // 优先：contextUsagePercentage 换算值
} else if inputTokens <= 0 {
    inputTokens = estimatedInputTokens // 兜底：本地预估
}
// 若上游 token 字段有值且 realInputTokens == 0，直接使用上游值
```

优先级：`contextUsage 换算` > `上游 token 字段` > `本地预估`

---

### 阶段④：确定最终 outputTokens（本地重新估算）

**Claude 路径**：

```go
outputTokens = estimateClaudeOutputTokens(outputContent, thinkingContent, toolUses)
// = estimateApproxTokens(outputContent)
//   + estimateApproxTokens(thinkingContent)
//   + Σ estimateApproxTokens(tu.Name) + estimateJSONTokens(tu.Input)
```

**OpenAI 流式路径**：

```go
outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
for _, tc := range toolCalls {
    outputTokens += estimateApproxTokens(tc.Function.Name)
    outputTokens += estimateApproxTokens(tc.Function.Arguments)
}
```

**OpenAI 非流式 / Responses API**：

```go
outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)
// 内部等同于 estimateClaudeOutputTokens
```

> **注意**：上游 EventStream 中的 outputTokens 仅在 Claude 路径的 `calibrateScaledUsage` 中作为基础值使用，最终上报值都来自本地重新估算，然后再由 credit 校准缩放。

---

### 阶段⑤：Prompt Cache Usage 计算（仅 Claude 路径）

在发出请求前，`promptCacheTracker` 模拟 Anthropic 的 prompt cache 行为，整个过程分三步。

```go
type promptCacheUsage struct {
    CacheCreationInputTokens   int  // 5m + 1h 之和（顶层扁平字段）
    CacheReadInputTokens       int  // 命中缓存的 token
    CacheCreation5mInputTokens int  // 新建 5min 缓存
    CacheCreation1hInputTokens int  // 新建 1h 缓存
}
```

#### 第一步：BuildClaudeProfile — 确定 breakpoints

遍历请求中所有 block（顺序：tools → system → messages），对每个 block 依次：

1. 将 block 内容做 canonical JSON 序列化（key 字典排序、剥除 `cache_control` 字段本身、剥除位置索引 key `tool_index` / `system_index` / `message_index` / `block_index`），喂入一个**持续滚动的 SHA-256 hasher**
2. 累加该 block 的估算 token 数
3. 判断是否为 breakpoint：
   - **显式**：block 上有 `cache_control: {type: "ephemeral"}`
   - **隐式**：已出现过显式 breakpoint 之后，每条消息的最后一个 block（`IsMessageEnd=true`）自动继承上一个显式 TTL

满足条件时，拍下当前 hasher 的快照作为 fingerprint（32 字节），连同 `CumulativeTokens` 和 TTL 记录为一个 breakpoint：

```
blocks:  [prelude][tool1][tool2*][sys1][msg0_b0][msg0_b1*][msg1_b0]
                           ↑                         ↑
                      explicit BP              implicit BP (IsMessageEnd，继承 TTL)

fingerprint = SHA256(所有前置 block 的 canonical 内容链快照)
```

特殊处理：
- `x-anthropic-billing-header:` 开头的 text block 整块跳过，不参与 fingerprint 也不计 token
- `cache_control` 字段在序列化时剥除，使不同 TTL 值的同内容 block 指纹相同

结果是一个 `promptCacheProfile{Breakpoints, TotalInputTokens, Model}`。

---

#### 第二步：Compute — 区分 creation / read

在请求发出**前**调用，决定本次的 cache 使用量：

**情形 A：首次请求（账号无历史缓存条目）**

```
lastTokens = min(lastBreakpoint.CumulativeTokens, TotalInputTokens)

effectiveCreation = lastTokens
if effectiveCreation < minTokens:
    effectiveCreation = 0    // 低于阈值，不上报 creation

→ CacheCreationInputTokens = effectiveCreation
→ CacheReadInputTokens     = 0
```

**情形 B：有历史缓存**

先截断 lastTokens：

```
lastTokens = min(lastBreakpoint.CumulativeTokens, TotalInputTokens)
lastTokens = min(lastTokens, TotalInputTokens × 0.85)   // 85% cap
```

从**最后一个 breakpoint 向前**扫描，找第一个 fingerprint 在账号缓存中存在且未过期的 breakpoint，命中时同步**续期**该条目：

```
matchedTokens = min(bp.CumulativeTokens, lastTokens)

→ CacheReadInputTokens    = matchedTokens
→ CacheCreationInputTokens = max(lastTokens - matchedTokens, 0)
```

约束条件：
- 最小可缓存阈值：普通模型 **1024 token**，Opus 模型 **4096 token**；低于阈值的 breakpoint 在扫描时跳过
- 85% cap：保证上报的缓存量不超过总 input 的 85%，避免"全部命中缓存"的不真实情况

---

#### 第三步：computePromptCacheTTLBreakdown — 拆分 5m / 1h

`CacheCreationInputTokens` 是 creation 总量，需进一步细分到 `ephemeral_5m` 和 `ephemeral_1h`。

从 `matchedTokens` 位置开始，按 breakpoint 顺序逐段累加**创建部分**：

```
previous = matchedTokens
for each breakpoint (CumulativeTokens 升序):
    current = min(bp.CumulativeTokens, TotalInputTokens)
    if current <= previous: skip
    delta = current - previous
    if bp.TTL >= 1h → cache1h += delta
    else            → cache5m += delta
    previous = current
```

`matchedTokens` 之前的段是 read，不参与拆分。

---

#### 第四步：Update — 写入账号缓存

响应成功后调用，将本次所有 breakpoint 写入账号的缓存 map：

```
entries[fingerprint] = {ExpiresAt: now + TTL, TTL: TTL}
```

低于最小 token 阈值的 breakpoint 跳过，不写入。下次请求的 `Compute` 步骤将从这里匹配。

---

#### 最终组装示例

```json
"cache_creation_input_tokens": 1800,
"cache_read_input_tokens": 3200,
"cache_creation": {
  "ephemeral_5m_input_tokens": 1800,
  "ephemeral_1h_input_tokens": 0
}
```

> `cache_creation_input_tokens`（顶层扁平字段）= `ephemeral_5m + ephemeral_1h`，二者是同一含义的冗余表达——扁平字段供旧版客户端使用，嵌套对象供支持多 TTL 的新版客户端使用。

---

### 阶段⑤b：Credit 校准（仅 Claude 路径，`calibrateScaledUsage`）

目标：使上报 usage 的 **列表价格合计 = upstream credits × CreditsToUSD**，同时 **保持 input_tokens 不变**（避免干扰客户端的 compaction 信号）。

#### 核心公式

```
billedInput  = inputTokens - CacheCreationInputTokens - CacheReadInputTokens
fixedCost    = billedInput × p_input
variableCost = outputTokens × p_output
             + CacheReadInputTokens × p_cache_read
             + CacheCreation5m × p_cache_create_5m
             + CacheCreation1h × p_cache_create_1h

target = credits × CreditsToUSD    // 默认 CreditsToUSD = 0.2

scale = (target - fixedCost) / variableCost

报告值：
  reportOutput = round(outputTokens × scale)
  CacheReadInputTokens    = round(原值 × scale)
  CacheCreation5mTokens   = round(原值 × scale)
  CacheCreation1hTokens   = round(原值 × scale)
  CacheCreationInputTokens = CacheCreation5m + CacheCreation1h (重新求和)
```

#### 跳过校准的情形

- `credits <= 0`（上游未返回计费事件）
- 模型定价表中无该模型的记录
- `variableCost <= 0`（无输出且无缓存活动）
- `scale <= 0`（固定输入成本已超过 target；上报警告日志，使用原始估算值）

#### 定价表来源

- 运行时缓存：`data/model_pricing.json`（由后台定时任务 `pricing_updater.go` 从上游拉取更新）
- 内嵌兜底：`proxy/model_pricing.json`（编译时通过 `//go:embed` 打包）

模型名查找顺序（`pricingLookupCandidates`）：
1. 原始名（如 `claude-opus-4.8`）
2. dot→dash 转换（`claude-opus-4-8`）
3. 经 `MapModel` alias 映射后的规范名
4. 映射后名再做 dot→dash 转换

---

## 三、各端点 usage 字段结构

### 3.1 Claude 流式（`/v1/messages` + `stream: true`）

**`message_start` 事件**（流开始时，使用预估值）：

```json
{
  "type": "message_start",
  "message": {
    "usage": {
      "input_tokens": <estimatedInputTokens>,
      "output_tokens": 0,
      // 若请求含 cache_control：
      "cache_creation_input_tokens": <CacheCreationInputTokens>,
      "cache_read_input_tokens": <CacheReadInputTokens>,
      "cache_creation": {
        "ephemeral_5m_input_tokens": <CacheCreation5mInputTokens>,
        "ephemeral_1h_input_tokens": <CacheCreation1hInputTokens>
      }
    }
  }
}
```

**`message_delta` 事件**（流结束时，使用校准后的最终值）：

```json
{
  "type": "message_delta",
  "usage": {
    "input_tokens": <billedInput>,     // = inputTokens - cacheCreation - cacheRead，不变
    "output_tokens": <reportOutput>,   // credit 校准后
    "cache_creation_input_tokens": <校准后>,
    "cache_read_input_tokens": <校准后>,
    "cache_creation": {
      "ephemeral_5m_input_tokens": <校准后>,
      "ephemeral_1h_input_tokens": <校准后>
    }
  }
}
```

> `input_tokens` 在流开始与结束时均不受 credit 校准影响，两次都使用固定值（一个是预估值，一个是 billedInput）。

---

### 3.2 Claude 非流式（`/v1/messages`）

响应体中的 `usage` 对象（`ClaudeUsage`）：

```json
{
  "usage": {
    "input_tokens": <billedInput>,
    "output_tokens": <reportOutput>,
    "cache_creation_input_tokens": <校准后，无缓存时省略>,
    "cache_read_input_tokens": <校准后，无缓存时省略>,
    "cache_creation": {
      "ephemeral_5m_input_tokens": <校准后>,
      "ephemeral_1h_input_tokens": <校准后>
    }
  }
}
```

---

### 3.3 OpenAI 流式（`/v1/chat/completions` + `stream: true`）

在流的最后一个 chunk（`finish_reason` 非空时）附带：

```json
{
  "usage": {
    "prompt_tokens": <inputTokens>,
    "completion_tokens": <outputTokens>,
    "total_tokens": <inputTokens + outputTokens>
  }
}
```

**无 credit 校准**，使用原始 inputTokens（contextUsage 换算 / 上游字段 / 本地预估）和本地重新估算的 outputTokens。

---

### 3.4 OpenAI 非流式（`/v1/chat/completions`）

响应体中的 `usage` 对象（`OpenAIUsage`）：

```json
{
  "usage": {
    "prompt_tokens": <inputTokens>,
    "completion_tokens": <outputTokens>,
    "total_tokens": <inputTokens + outputTokens>
  }
}
```

同样**无 credit 校准**。

---

### 3.5 OpenAI Responses API（`/v1/responses`）

响应体中的 `usage` 对象（`ResponsesUsage`）：

```json
{
  "usage": {
    "input_tokens": <inputTokens>,
    "output_tokens": <outputTokens>,
    "total_tokens": <inputTokens + outputTokens>
  }
}
```

**无 credit 校准**，无 cache 字段。

---

## 四、上报值与内部统计的分离

usage 字段（对外）与内部统计（`recordSuccessForApiKey`、`pool.UpdateStats`）使用**不同的值**：

| 用途 | inputTokens | outputTokens |
|------|-------------|--------------|
| 内部统计 / 账号用量 | contextUsage 换算 or 上游字段 or 本地预估（原始值） | 本地重新估算（校准前） |
| Claude 响应 usage 字段 | billedInput（固定，不受校准影响） | reportOutput（credit 校准后） |
| OpenAI 响应 usage 字段 | inputTokens（原始值） | outputTokens（本地估算，无校准） |

---

## 五、配置项

| 配置字段 | 位置 | 默认值 | 说明 |
|----------|------|--------|------|
| `creditsToUSD` | `config.json > settings` | `0.2` | upstream credits → USD 转换系数，影响 credit 校准的 target |
| `cache_control.ttl` | 请求 block 中 | `5m` | `"ephemeral"` 类型的缓存 TTL，超过 5min 则按 1h 档处理 |

---

## 六、调用关系总览

```
handler.go
  ├── estimateClaudeRequestInputTokens()     [token_estimator.go] → estimatedInputTokens
  ├── promptCache.BuildClaudeProfile()       [cache_tracker.go]   → cacheProfile
  ├── promptCache.Compute()                  [cache_tracker.go]   → cacheUsage (promptCacheUsage)
  ├── CallKiroAPI()                          [kiro.go]
  │     └── updateTokensFromEvent()          [kiro.go]            → inputTokens, outputTokens
  │         meteringEvent                                          → credits
  │         contextUsageEvent                                      → realInputTokens
  ├── estimateClaudeOutputTokens()           [token_estimator.go] → outputTokens (重新估算)
  ├── billedClaudeInputTokens()              [pricing.go]         → billedInput
  ├── calibrateScaledUsage()                 [pricing.go]         → reportOutput, reportUsage
  │     └── lookupModelPricing()             [pricing.go]
  │           └── pricingLookupCandidates()  [pricing.go]
  ├── buildClaudeUsageMap()                  [pricing.go]         → message_start usage
  └── buildClaudeUsageMapExplicit()          [pricing.go]         → message_delta usage
```
