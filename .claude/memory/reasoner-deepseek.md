---
name: reasoner-deepseek
kind: decision
importance: 3
tags: [reasoner, deepseek, llm]
---

**DeepSeek chat** (OpenAI-совместимый, ключ в env `DEEPSEEK_API_KEY`) — в роли `Reasoner`:
суммаризация recall-выдачи, консолидация episodic→semantic, дедуп-суждения.

**Почему:** у пользователя есть ключ DeepSeek; это chat-задачи, где DeepSeek применим.
**Как применять:** DeepSeek — ТОЛЬКО для текстовых LLM-операций, НЕ для эмбеддингов
(embeddings endpoint у DeepSeek отсутствует — вектор берём у ollama). Вызовы — в пакете
`reason/` за интерфейсом `Reasoner`; недоступность LLM не должна ломать основной флоу.

Связано: [[embedder-ollama]]
