# 渠道响应模型名

新版前端：渠道 → 编辑 → 高级设置 → 渠道额外设置 → **响应使用请求模型名**。

该开关默认关闭，保存在渠道 `setting` JSON 的 `response_model_name` 字段中。开启后，响应中的模型标识使用客户端请求的名称。例如：

```json
{
  "response_model_name": true
}
```

模型映射仍按请求名称到上游名称配置：

```json
{
  "openai/gpt-6-astra": "braintrust/gpt-6-astra"
}
```

开启后的响应模型名为 `openai/gpt-6-astra`。开关不会修改发给上游的模型、计费数据或模型映射。

## 覆盖范围

| 协议 | 回写字段 |
| --- | --- |
| Chat Completions、Completions | 普通响应和 SSE 分块的 `model` |
| Messages | 普通响应的 `model`、`message_start` 的 `message.model` |
| Responses、Responses Compact | 普通响应的 `model`、流式事件的 `response.model` |
| Embeddings、图片、音频 JSON、Rerank | 响应中已有的顶层 `model` |
| Gemini | `modelVersion`，以及已有的顶层 `model` |
| Realtime | `session.model`、`response.model`，以及已有的顶层 `model` |

HTTP 处理统一位于同步 relay 输出出口，适用于各提供商及协议转换后的结果。异步任务接口使用原有的任务响应处理流程。

只修改协议指定位置的字符串字段；不添加缺失字段，不递归替换正文、工具参数、元数据或错误信息。非 JSON 的音频和其他二进制内容原样返回。渠道连接测试也应用该开关；重试时使用实际选中渠道的配置。

## 流式行为与验证

SSE 按完整事件处理并立即交给原有输出流程，不缓存完整回答。事件名称、ID、结束标记和心跳保持有效。普通 JSON 必要时暂存跨写入的片段，回写时清除失效的 Content-Length。

后端回归测试覆盖模型字段、内容和用量保留、关闭行为、跨写入 JSON/SSE、CRLF、多行 SSE、二进制、错误、重试切换及 HTTP 报文长度。前端测试覆盖新建、重新打开和关闭开关后的保存。

```sh
go test ./relay/... ./controller ./dto ./service/relayconvert/...
go test -race ./relay/common -run TestResponseModel
```

在 `web/default/` 执行：

```sh
bun test src/features/channels/lib/channel-form.test.ts
bun run typecheck
bun run build
```
