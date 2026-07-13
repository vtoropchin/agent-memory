// Package mcp регистрирует инструменты AgentMemory для агента и обслуживает их по
// протоколу MCP (DESIGN.md §3, §12). Пакет инкапсулирует весь MCP-специфичный код:
// наружу торчит только Server с конструктором и Run — остальные пакеты (write, recall,
// store, config) о MCP не знают.
package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// serverName/serverVersion — идентификация сервера в MCP-рукопожатии.
const (
	serverName    = "memo"
	serverVersion = "0.1.0"
)

// Server — MCP-сервер AgentMemory: держит уже собранные Writer/Recaller/Store и
// регистрирует поверх них ровно 4 инструмента (DESIGN.md §12): memory_save,
// memory_recall, memory_update, memory_forget.
type Server struct {
	sdk      *sdkmcp.Server
	writer   *write.Writer
	recaller *recall.Recaller
	store    store.Store
	scope    config.ScopeConfig
}

// NewServer создаёт MCP-сервер поверх уже сконструированных зависимостей и
// регистрирует инструменты. scope задаёт дефолты save/recall инстанса (DESIGN.md
// §2.1) — приоритет всегда у явного scope из вызова инструмента. Транспорт не
// запускается — для этого вызови Run.
func NewServer(w *write.Writer, r *recall.Recaller, s store.Store, scope config.ScopeConfig) *Server {
	srv := &Server{
		sdk:      sdkmcp.NewServer(&sdkmcp.Implementation{Name: serverName, Version: serverVersion}, &sdkmcp.ServerOptions{Instructions: buildInstructions(scope)}),
		writer:   w,
		recaller: r,
		store:    s,
		scope:    scope,
	}
	srv.registerTools()
	return srv
}

// registerTools добавляет все инструменты AgentMemory на sdk-сервер.
func (s *Server) registerTools() {
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{
		Name:        "memory_save",
		Description: "Сохранить атомарный факт/событие/процедуру в долговременную память агента.",
		InputSchema: saveInputSchema(),
	}, s.handleSave)

	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{
		Name:        "memory_recall",
		Description: "Найти релевантную память по смыслу запроса и вернуть компактный Markdown-бриф, сгруппированный по типу и обрезанный под бюджет токенов.",
		InputSchema: recallInputSchema(),
	}, s.handleRecall)

	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{
		Name:        "memory_update",
		Description: "Изменить существующий факт: содержание, краткую форму, важность или теги. Меняются только переданные поля.",
		InputSchema: updateInputSchema(),
	}, s.handleUpdate)

	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{
		Name:        "memory_forget",
		Description: "Мягко удалить запись из памяти (тумстоун): она перестаёт участвовать в recall, но история сохраняется. Опционально указать id заменяющей записи.",
		InputSchema: forgetInputSchema(),
	}, s.handleForget)
}

// Run запускает MCP-сервер поверх stdio-транспорта и блокируется до отключения
// клиента или отмены ctx. Весь ввод/вывод протокола идёт через stdin/stdout —
// вызывающий код не должен писать в stdout ничего постороннего (см. cmd/memo).
func (s *Server) Run(ctx context.Context) error {
	if err := s.sdk.Run(ctx, &sdkmcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp: работа сервера: %w", err)
	}
	return nil
}

// textResult оборачивает text в единственный текстовый блок CallToolResult —
// стандартный способ вернуть человекочитаемый ответ инструмента (DESIGN.md §12).
func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}
}
