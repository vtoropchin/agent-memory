# MEMORY — индекс памяти проекта

Один-строчные указатели на записи в `.claude/memory/`. Формат: `- [Название](file.md) — крючок`.

- [Стек и хранилище](stack-and-storage.md) — Go + embedded SQLite (FTS5 + sqlite-vec), ноль внешних сервисов
- [Транспорт — MCP](transport-mcp.md) — агент ходит в память через MCP over stdio
- [Embedder — ollama/bge-m3](embedder-ollama.md) — локальный вектор, конфигурируемая модель, автоопределение dim
- [Reasoner — DeepSeek](reasoner-deepseek.md) — DeepSeek chat для суммаризации/консолидации; НЕ для эмбеддингов
- [Write path — структуру даёт агент](write-structured.md) — агент передаёт готовый факт с полями
- [Прогресс проекта](progress.md) — на каком этапе roadmap мы находимся
- [Грабли и тех-долг MVP](mvp-notes.md) — build-флаги, FTS self-contained, тумстоун-forget, лексич. ограничение recall
- [Задачи — по файлу на задачу](task-files.md) — бэклог в tasks/*.md, делегирование ссылкой на файл, индекс tasks/README.md
