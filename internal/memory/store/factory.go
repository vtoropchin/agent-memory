// factory.go — выбор реализации Store по конфигу (DESIGN.md §13, драйвер
// store.driver).
package store

import (
	"fmt"

	"agentmemory/internal/config"
)

// Open — фабрика Store: "" и "sqlite" (дефолт) открывают файл SQLite через
// OpenSQLite(cfg.Path); "postgres" — соединение с PostgreSQL+pgvector через
// openPostgres (postgres.go). Неизвестный driver — ошибка: config.Load уже
// отклоняет такой конфиг через StoreConfig.Validate до того, как он попадёт
// сюда, но фабрика остаётся defensive для вызовов в обход Load (тесты,
// сторонний код пакета config).
func Open(cfg config.StoreConfig) (Store, error) {
	switch cfg.Driver {
	case "", "sqlite":
		return OpenSQLite(cfg.Path)
	case "postgres":
		return openPostgres(cfg)
	default:
		return nil, fmt.Errorf("store: неизвестный driver %q", cfg.Driver)
	}
}
