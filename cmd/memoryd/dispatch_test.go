// dispatch_test.go — регрессия БАГ 2 цикла исправлений задачи 11.1: миграция
// диспетчера подкоманд на cobra сломала маршрутизацию `memoryd -db recall`/
// `memoryd -config web` (флаг сервера, чьё значение совпадает с именем
// подкоманды) — cobra.Command.Find (внутри root.Execute) сама решает, что
// считать подкомандой, через эвристику stripFlags, которая в этих случаях
// отбрасывает сам флаг "-db"/"-config" и трактует его значение как имя
// подкоманды. Экспериментально (см. историю разработки задачи) подтверждено:
//
//	root.Find([]string{"-db", "recall"})                      -> cmd="recall"  (БАГ)
//	root.Find([]string{"-config", "web"})                     -> cmd="web"     (БАГ)
//	root.Find([]string{"-config", "/proj/agentmemory.toml"})  -> cmd="memoryd" (ок, основной путь .mcp.json не задет)
//	root.Find([]string{"bogus"})                               -> cmd="memoryd" (ок, неизвестное имя и раньше падало на сервер)
//
// decideRoute/runMain (main.go) чинят это, полностью обходя cobra.Find для
// аргументов, похожих на флаг (начинаются с "-") — такие args вообще не
// попадают в root.Execute(), поэтому разойтись с историческим поведением
// (старый диспетчер матчил os.Args[1] точно по имени подкоманды) нечем.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDecideRoute проверяет решение маршрутизации для всех сценариев из
// цикла исправлений БАГ 2: флаг сервера (в т.ч. совпадающий с именем
// подкоманды) -> routeServer, help-токен -> routeHelp, имя подкоманды (в т.ч.
// неизвестное — регресс сохранён) -> routeCobra.
func TestDecideRoute(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want routeDecision
	}{
		{"-db со значением, совпадающим с именем подкоманды recall", []string{"-db", "recall"}, routeServer},
		{"-config со значением, совпадающим с именем подкоманды web", []string{"-config", "web"}, routeServer},
		{"-config со значением, совпадающим с именем подкоманды import", []string{"-config", "import"}, routeServer},
		{"-config с обычным путём к TOML (основной путь .mcp.json)", []string{"-config", "/proj/agentmemory.toml"}, routeServer},
		{"-db с обычным путём к БД", []string{"-db", "/some/memory.db"}, routeServer},
		{"голый вызов без аргументов", nil, routeCobra},
		{"известная подкоманда с позиционным аргументом", []string{"recall", "запрос"}, routeCobra},
		{"неизвестное имя подкоманды — поведение сохранено (-> сервер через root)", []string{"bogus"}, routeCobra},
		{"-h", []string{"-h"}, routeHelp},
		{"-help", []string{"-help"}, routeHelp},
		{"--help", []string{"--help"}, routeHelp},
		{"help", []string{"help"}, routeHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideRoute(tt.args); got != tt.want {
				t.Errorf("decideRoute(%v) = %v, хочу %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRunMain_HelpRoute проверяет, что runMain с help-токеном печатает
// writeUsage в stdout и возвращает 0, не обращаясь ни к cobra, ни к серверу.
func TestRunMain_HelpRoute(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain(strings.NewReader(""), &stdout, &stderr, []string{"-h"})
	if code != 0 {
		t.Errorf("код выхода = %d, хочу 0", code)
	}
	if !strings.Contains(stdout.String(), "memoryd init") {
		t.Errorf("stdout не похож на writeUsage(): %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, хочу пусто", stderr.String())
	}
}

// TestRunMain_CobraRoute_KnownSubcommand проверяет, что известное имя
// подкоманды в runMain действительно доходит до неё (recall без запроса ->
// ошибка recall с характерным префиксом), а не до серверного пути — то есть
// routeCobra реально передаёт args в cobra, а не молча проглатывает их.
func TestRunMain_CobraRoute_KnownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain(strings.NewReader(""), &stdout, &stderr, []string{"recall"})
	if code != 1 {
		t.Errorf("код выхода = %d, хочу 1 (recall без запроса — ошибка)", code)
	}
	if !strings.Contains(stderr.String(), "memoryd recall:") {
		t.Errorf("stderr = %q, хочу префикс \"memoryd recall:\" (подтверждает маршрут в подкоманду recall)", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, хочу пусто", stdout.String())
	}
}

// TestRootFind_KnownCobraQuirk_DoesNotAffectRunMain документирует сам баг
// cobra.Command.Find на уровне root.Find (без выполнения — Execute() здесь не
// вызывается, поэтому сервер не стартует и тест не блокируется на stdio):
// "-db recall"/"-config web" резолвятся Find в подкоманды recall/web. Это НЕ
// регрессия runMain (decideRoute перехватывает такие args раньше, чем они
// попадают в cobra, см. TestDecideRoute) — тест существует, чтобы явно
// зафиксировать, почему обход cobra.Find необходим, а не полагаться на неё.
func TestRootFind_KnownCobraQuirk_DoesNotAffectRunMain(t *testing.T) {
	exitCode := 0
	root := newRootCmd(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, &exitCode)

	if cmd, _, err := root.Find([]string{"-db", "recall"}); err != nil || cmd.Name() != "recall" {
		t.Fatalf("root.Find(-db, recall) = %q, err=%v — квирк cobra изменился, перепроверь decideRoute", cmd.Name(), err)
	}
	if cmd, _, err := root.Find([]string{"-config", "web"}); err != nil || cmd.Name() != "web" {
		t.Fatalf("root.Find(-config, web) = %q, err=%v — квирк cobra изменился, перепроверь decideRoute", cmd.Name(), err)
	}
}
