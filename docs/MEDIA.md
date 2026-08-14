# 语音 / 图像 / 识图能力与用法

> 更新时间: 2026-08-14。aurora 的媒体类能力分三块:语音转写(TTS/ASR)、
> 图像生成、识图(视觉对话)。本文记录各通道现状与调用方式。

## 一、语音转写(/v1/audio/transcriptions,OpenAI 标准接口)

**通用接口**,multipart/form-data,`model` 字段决定通道:

| model | 通道 | 状态 |
|---|---|---|
| `whisper-1` | ChatGPT 网页转录(需登录账号池) | ✅ 可用(实测) |
| `mimo-v2.5-asr` | 小米 Mimo 网页 ASR | ✅ 可用(实测,见 docs/MIMO.md) |

```bash
# ChatGPT whisper
curl http://10.10.10.2:65432/v1/audio/transcriptions \
  -H "Authorization: Bearer david" \
  -F "file=@test.wav;type=audio/wav" \
  -F "model=whisper-1"

# 小米 Mimo ASR
curl http://10.10.10.2:65432/v1/audio/transcriptions \
  -H "Authorization: Bearer david" \
  -F "file=@test.wav;type=audio/wav" \
  -F "model=mimo-v2.5-asr"

# 中文识别可加 language;返回格式可选 response_format=json|text|verbose_json
```

**语音合成(/v1/audio/speech)**:ChatGPT 网页 TTS,模型 `gpt-4o-mini-tts` 等
(输入 `input` 文本 + `voice`,输出音频文件)。

**国内 ASR 未接入通道**(2026-08-14 盘点):阿里 Paraformer/SenseVoice(百炼有免费额度)、
字节豆包 Seed-ASR(火山引擎)、MiniMax ASR、讯飞、百度、腾讯云 —— 均建议走官方 API
(免费额度 + 无封号风险),网页端逆向仅 Mimo 已做。

## 二、图像生成(/v1/images/generations)

| 通道 | 状态 |
|---|---|
| ChatGPT 网页(gpt-image 系列) | ✅ 可用 |
| 国内即梦/万相/文心/混元/MiniMax image-01 | ❌ 未接入(建议官方 API) |

## 三、识图(视觉对话)

**用法**:标准 OpenAI 多模态 content parts,`image_url` 可传 http(s) URL 或 data: URI:

```bash
curl http://10.10.10.2:65432/v1/chat/completions \
  -H "Authorization: Bearer david" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash-chat",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "这张图片里有什么?"},
        {"type": "image_url", "image_url": {"url": "https://example.com/photo.jpg"}}
      ]
    }]
  }'
```

**通道现状(如实标注)**:

| provider | 识图 | 说明 |
|---|---|---|
| ChatGPT(auto/gpt-*) | ✅ | 网页原生多模态,image_url 直接透传 |
| DeepSeek(-chat) | ✅ | aurora 自动上传网页(upload_file→fork vision)→ model_type=vision |
| Gemini(-chat) | ❌ | CDP 桥只传文本,图片被丢弃(能力标注已移除) |
| Claude(-chat) | ❌ | 同上(网页有 attachments 字段但桥未实现上传) |
| Kimi / GLM / Grok / 豆包 / 千问 / MiniMax / Mimo | ❌ | 未实现识图上传 |

> 若要补 Gemini/Claude 识图,需在桥的适配器里实现"图片上传→附件引用"流程
> (Gemini 走会话附件、Claude 走 attachments 字段),工作量中等,按需再做。
