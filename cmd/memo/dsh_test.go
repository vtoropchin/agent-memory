// dsh_test.go — регрессии на владение блоком оверлея dsh: memo обновляет
// только свой блок и не теряет сторонние строки, попавшие внутрь него.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dshTestTemplate — корректный блок оверлея memo для каталога dir: тесты
// строят слой из настоящего рендера, чтобы шаблон в тесте не разошёлся с
// dsh.go. memoPath отличает старый блок от нового.
func dshTestTemplate(dir, memoPath string) string {
	return renderDshBlock(memoPath, dir, initConfig{}, false)
}

// dshTestWithForeign дописывает чужую операцию перед закрывающим маркером
// блока — так выглядит правка, которую пользователь сделал руками внутри
// блока memo.
func dshTestWithForeign(t *testing.T, block, dir, foreign string) string {
	t.Helper()
	end := dshBlockEnd(dir) + "\n"
	if !strings.HasSuffix(block, end) {
		t.Fatalf("тестовый блок не заканчивается маркером конца %q:\n%s", end, block)
	}
	return strings.TrimSuffix(block, end) + foreign + end
}

// TestInstallDshPatch_KeepsForeignLines — регрессия: сторонняя операция
// верхнего уровня, дописанная внутри блока memo перед закрывающим маркером,
// раньше молча удалялась (installDshPatch собирал текст только вне спанов).
// Теперь она сохраняется, init предупреждает, а собственный блок обновляется.
func TestInstallDshPatch_KeepsForeignLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := t.TempDir()
	layerPath := filepath.Join(home, "cordis.patch.yml")

	oldBlock := dshTestTemplate(dir, "/opt/memo-old")
	foreign := "- insert:\n  - id: foreign-op\n    name: foreign\n"
	layer := dshTestWithForeign(t, oldBlock, dir, foreign)
	if err := os.WriteFile(layerPath, []byte(layer), 0o600); err != nil {
		t.Fatal(err)
	}

	block := dshTestTemplate(dir, "/opt/memo-new")
	var out strings.Builder
	connected, err := installDshPatch(dir, block, &out)
	if err != nil {
		t.Fatalf("installDshPatch: %v", err)
	}
	if !connected {
		t.Fatalf("блок не подключён: %s", out.String())
	}

	raw, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "id: foreign-op") {
		t.Errorf("чужая операция потеряна:\n%s", got)
	}
	if strings.Contains(got, "/opt/memo-old") || !strings.Contains(got, "/opt/memo-new") {
		t.Errorf("блок memo не обновлён:\n%s", got)
	}
	if n := strings.Count(got, dshMarkerStartPrefix); n != 1 {
		t.Errorf("блоков memo в слое %d, жду один:\n%s", n, got)
	}
	if !strings.Contains(out.String(), "строки вне шаблона") {
		t.Errorf("вывод без предупреждения о сохранённых строках: %s", out.String())
	}
	if i, j := strings.Index(got, "id: foreign-op"), strings.Index(got, dshBlockStart(dir)); i < 0 || i > j {
		t.Errorf("чужая операция не осталась на своём месте (foreign=%d, блок=%d):\n%s", i, j, got)
	}

	// Повторный прогон того же init идемпотентен: файл не переписывается.
	var again strings.Builder
	connected, err = installDshPatch(dir, block, &again)
	if err != nil {
		t.Fatalf("повторный installDshPatch: %v", err)
	}
	if !connected || !strings.Contains(again.String(), "уже подключён") {
		t.Errorf("повторный прогон: connected=%v вывод=%s", connected, again.String())
	}
	rawAgain, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя после повтора: %v", err)
	}
	if string(rawAgain) != got {
		t.Errorf("повторный init изменил слой:\nбыло:\n%s\nстало:\n%s", got, rawAgain)
	}
}

// TestInstallDshPatch_SwitchKeepsForeignLines — смена проекта по-прежнему
// удаляет блок прежнего проекта и предупреждает о переключении, но чужие
// строки внутри этого блока не теряются.
func TestInstallDshPatch_SwitchKeepsForeignLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	layerPath := filepath.Join(home, "cordis.patch.yml")
	foreign := "- insert:\n  - id: foreign-op\n"
	layer := dshTestWithForeign(t, dshTestTemplate(firstDir, "/opt/memo"), firstDir, foreign)
	if err := os.WriteFile(layerPath, []byte(layer), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	connected, err := installDshPatch(secondDir, dshTestTemplate(secondDir, "/opt/memo"), &out)
	if err != nil || !connected {
		t.Fatalf("installDshPatch: connected=%v err=%v", connected, err)
	}
	raw, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, dshBlockStart(firstDir)) || !strings.Contains(got, dshBlockStart(secondDir)) {
		t.Errorf("слой не переключён на второй проект:\n%s", got)
	}
	if !strings.Contains(got, "id: foreign-op") {
		t.Errorf("чужая операция прежнего блока потеряна при переключении:\n%s", got)
	}
	if !strings.Contains(out.String(), "память dsh переключена") || !strings.Contains(out.String(), "строки вне шаблона") {
		t.Errorf("вывод без предупреждений о переключении и сохранённых строках: %s", out.String())
	}
}

// TestInstallDshPatch_UnrecognizedTemplateKeepsFile — блок внутри маркеров, чей
// шаблон memo не распознала (вставлен не init или в другой форме), не
// принадлежит memo: файл не меняется, печатается предупреждение.
func TestInstallDshPatch_UnrecognizedTemplateKeepsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := t.TempDir()
	layerPath := filepath.Join(home, "cordis.patch.yml")
	layer := dshBlockStart(dir) + "\n- insert:\n    - id: custom\n" + dshBlockEnd(dir) + "\n"
	if err := os.WriteFile(layerPath, []byte(layer), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	connected, err := installDshPatch(dir, dshTestTemplate(dir, "/opt/memo"), &out)
	if err != nil {
		t.Fatalf("installDshPatch: %v", err)
	}
	if connected {
		t.Errorf("блок с нераспознанным шаблоном подключён")
	}
	raw, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя: %v", err)
	}
	if string(raw) != layer {
		t.Errorf("слой с нераспознанным шаблоном изменён:\n%s", raw)
	}
	if !strings.Contains(out.String(), "шаблон блока memo не распознан") {
		t.Errorf("вывод без предупреждения о нераспознанном шаблоне: %s", out.String())
	}
}

// TestReplaceDshBlock_KeepsForeignLines — profile-слой (replaceDshBlock):
// строки внутри блока, которых memo не писала, сохраняются после нового блока
// и возвращаются вызывающему для предупреждения; повторная замена того же
// блока файл не меняет.
func TestReplaceDshBlock_KeepsForeignLines(t *testing.T) {
	dir := "/p"
	block := dshTestTemplate(dir, "/opt/memo-new")
	foreign := "- insert:\n  - id: foreign-op\n"
	old := dshTestWithForeign(t, dshTestTemplate(dir, "/opt/memo-old"), dir, foreign)
	text := "before\n" + old + "after\n"

	got, found, preserved, err := replaceDshBlock(text, dir, block)
	if err != nil || !found {
		t.Fatalf("replaceDshBlock: found=%v err=%v", found, err)
	}
	if want := "before\n" + block + foreign + "after\n"; got != want {
		t.Errorf("замена блока:\n%s\nхочу:\n%s", got, want)
	}
	if len(preserved) == 0 || !strings.Contains(strings.Join(preserved, ""), "id: foreign-op") {
		t.Errorf("чужие строки не возвращены вызывающему: %q", preserved)
	}

	again, found, preservedAgain, err := replaceDshBlock(got, dir, block)
	if err != nil || !found {
		t.Fatalf("повторный replaceDshBlock: found=%v err=%v", found, err)
	}
	if again != got {
		t.Errorf("повторная замена изменила текст:\nбыло:\n%s\nстало:\n%s", got, again)
	}
	if len(preservedAgain) != 0 {
		t.Errorf("повторная замена снова нашла чужие строки: %q", preservedAgain)
	}
}

// TestSyncDshProfileCopies_KeepsForeignLines — профильный слой: init обновляет
// копию блока, сохраняя чужие строки внутри него и предупреждая о них; второй
// проход файл не трогает.
func TestSyncDshProfileCopies_KeepsForeignLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := t.TempDir()
	layerPath := filepath.Join(home, "profiles", "desktop", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(layerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "- insert:\n  - id: foreign-op\n"
	layer := "- id: head\n" + dshTestWithForeign(t, dshTestTemplate(dir, "/opt/memo-old"), dir, foreign)
	if err := os.WriteFile(layerPath, []byte(layer), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	block := dshTestTemplate(dir, "/opt/memo-new")
	if err := syncDshProfileCopies(dir, block, &out); err != nil {
		t.Fatalf("syncDshProfileCopies: %v", err)
	}
	raw, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя профиля: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "id: foreign-op") {
		t.Errorf("чужая операция потеряна из слоя профиля:\n%s", got)
	}
	if !strings.Contains(got, "/opt/memo-new") || strings.Contains(got, "/opt/memo-old") {
		t.Errorf("блок в слое профиля не обновлён:\n%s", got)
	}
	if !strings.Contains(got, "id: head") {
		t.Errorf("остальные строки слоя профиля потеряны:\n%s", got)
	}
	if !strings.Contains(out.String(), "строки вне шаблона") || !strings.Contains(out.String(), "обновлён блок memo") {
		t.Errorf("вывод без предупреждения/сообщения об обновлении: %s", out.String())
	}
	if info, err := os.Stat(layerPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("права слоя профиля = %v (err=%v), хочу 0600", info.Mode().Perm(), err)
	}

	var again strings.Builder
	if err := syncDshProfileCopies(dir, block, &again); err != nil {
		t.Fatalf("повторный syncDshProfileCopies: %v", err)
	}
	rawAgain, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("чтение слоя профиля после повтора: %v", err)
	}
	if string(rawAgain) != got || again.Len() != 0 {
		t.Errorf("повторный проход изменил слой (вывод %q):\n%s", again.String(), rawAgain)
	}
}
