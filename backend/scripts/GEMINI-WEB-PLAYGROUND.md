# Gemini 网页版 Playground

Gemini 网页版集成在现有 Gemini / Playground 账号类型中，账号凭据的 `playground_backend` 区分 `aistudio` 和 `gemini_web`。已有账号缺少该字段时按 AI Studio 处理。

## 操作

1. 新建账号，选择 **Gemini → Playground（网页会话）**，填写名称、代理和分组。
2. 下一步在 **Playground 来源** 选择 **Gemini 网页版**，打开登录窗口。
3. 在 Camoufox 内完成 Google 登录及 Gemini 首次引导，进入 `gemini.google.com/app` 后点击 **完成登录**，再保存账号。
4. 使用账号的模型同步或测试功能查看该账号实际模型。常见名称为 `gemini-flash`、`gemini-pro`、`gemini-flash-lite`；只有账号实际返回的模型可以调用。配置别名时把目标映射到这些网页模型名称。
5. 将现有客户端的 API Key 绑定到上述 Gemini 分组。继续使用本项目原来的 API 地址。

编辑账号可切换来源，但必须完成新来源的浏览器登录。登录授权同时绑定来源和代理，无法把 AI Studio 授权用于 Gemini 网页版。账号列表显示具体来源。

批量上号也有相同的来源选择；同一邮箱可分别建立 AI Studio 和 Gemini 网页版账号。批次内的来源固定。Gemini 新版首次引导无法识别时转人工处理。

ZIP 导出包含浏览器 Profile 和来源字段。导入时恢复新的 Profile，再签发同来源的一次性授权。部署仍使用已有的 `PLAYGROUND_PROFILE_DIR` / `DATA_DIR`、Camoufox、Redis 和代理配置。

## 请求链路

```text
Chat Completions / Responses / Messages / Gemini 原生请求
  → 本项目已有的 Gemini 请求转换
  → PlaygroundGatewayService 按账号来源分流
  → 获取 Profile 租约和 Redis Cookie 会话
  → Gemini /app 页面令牌与账号模型发现
  → 网页 StreamGenerate RPC
  → 增量解码为 Gemini SSE
  → 本项目已有的各协议响应转换
```

网页适配器复用现有代理、浏览器指纹、响应 Cookie 更新、Redis 租约和浏览器恢复机制。认证失败在输出前最多尝试一次浏览器恢复；生成请求总预算五分钟，客户端关闭流会中止请求。网页返回的 RPC 错误在首个有效事件之前转换成 HTTP 错误，避免配额拒绝表现为空的成功流。

Gemini 网页版不执行 AI Studio WAA / SpiderMonkey 证明，也不参加 AI Studio WAA 预热。

## 支持范围

| 能力 | 当前行为 |
| --- | --- |
| OpenAI Chat Completions、Responses | 流式和非流式，复用现有兼容层 |
| Anthropic Messages、Gemini generateContent | 流式和非流式，复用现有兼容层 |
| 多轮与系统指令 | 每次发送完整历史，以角色分隔的提示词表达；不依赖隐藏的网页会话 ID |
| 思考 | 支持关闭、低、中、高；只在客户端要求时返回思考内容 |
| 输入图片/文件 | `inlineData` 经同一账号及代理上传；每个文件最多 20 MiB、每次最多 20 个，总量最多 50 MiB；上传失败则请求失败 |
| 函数调用 | 提示词声明函数和 JSON 返回约定，校验函数名称后转换为协议函数调用；有函数声明时正文缓冲到生成完成 |
| 网页模型列表 | 从账号 `GetUserStatus` 返回值解析，不为账号补造未发现的模型 |
| countTokens | 本地估算；网页版不提供精确 tokenizer |
| 用量 | 网页不提供官方 token 计数，适配器不构造虚假的 usageMetadata；上层既有估算策略仍可能生效 |
| 生成采样参数 | 网页控制实际采样，不保证 `temperature`、`top_p`、`seed`、`max_tokens` 等官方 API 参数等价 |
| JSON 输出 | 通过提示词表达 MIME 类型和 Schema，无法保证官方受限解码语义 |
| 图片输出、Deep Research、Gems、Canvas | 当前适配器不实现；图片输出请求明确报错，图片生成继续使用 AI Studio Playground |
| cachedContent、fileData、代码执行 | 明确拒绝；文件需转为 `inlineData` |

网页上下文和配额来自该 Google 账号的实际权限，不继承 AI Studio 的上下文承诺。网页模型稳定名称表示产品族，不保证与同名官方 API 模型版本相同。

## 参考与验证

协议研究参考 [xwteam/gemini2api](https://github.com/xwteam/gemini2api)，包括页面令牌、RPC 字段、模型发现和累积文本响应的行为。该项目采用 [PolyForm Noncommercial 1.0.0](https://github.com/xwteam/gemini2api/blob/main/LICENSE)。本实现没有嵌入或启动该 Python 项目；Go 适配器使用本项目现有会话与网关架构。

自动化验证使用合成页面、模型目录和 RPC 响应，覆盖四种协议的流式/非流式、来源授权绑定、跨来源隔离、附件、思考和函数调用转换。真实 Google 登录、当前账号模型权限、代理可达性及 Google 当前网页协议仍需在部署环境联调。
