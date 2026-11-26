// Пакет main — сборка поставочного архива плагина.
// Инструмент упаковывает собранную библиотеку gigacode.so в zip-архив
// формата магазина плагинов CLIProxyAPI (схема github-release):
// в корне архива — один файл gigacode.so с правами 0755, рядом с архивом
// — checksums.txt в формате sha256sum. Только стандартная библиотека.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Суффикс имени архива: GOOS_GOARCH фиксированы — плагин собирается
// только под linux/amd64.
const archivePattern = "gigacode_%s_linux_amd64.zip"

// Имя библиотеки в корне архива: магазин ищет <id>.so по id плагина.
const libraryName = "gigacode.so"

// Права библиотеки в архиве: хост переносит их на установленный файл.
const libraryMode = 0o755

// Валидная версия: тот же шаблон, что проверяет магазин плагинов.
var versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

func run() error {
	version := flag.String("version", "", "версия плагина, например 0.1.0")
	in := flag.String("in", "", "путь к собранной библиотеке gigacode.so")
	out := flag.String("out", "", "каталог для архива и checksums.txt")
	flag.Parse()

	if !versionRe.MatchString(strings.TrimSpace(*version)) {
		return fmt.Errorf("некорректная -version %q: ожидается шаблон ^[0-9][0-9A-Za-z.+-]*$", *version)
	}
	if strings.TrimSpace(*in) == "" {
		return fmt.Errorf("не задан -in: путь к gigacode.so")
	}
	if strings.TrimSpace(*out) == "" {
		return fmt.Errorf("не задан -out: каталог результата")
	}
	return packagePlugin(*version, *in, *out)
}

// packagePlugin создаёт архив и checksums.txt в каталоге out.
func packagePlugin(version, in, out string) error {
	library, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("прочитать %s: %w", in, err)
	}
	if len(library) == 0 {
		return fmt.Errorf("пустой файл библиотеки: %s", in)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("создать каталог %s: %w", out, err)
	}

	archivePath := filepath.Join(out, fmt.Sprintf(archivePattern, version))
	if err := writeArchive(archivePath, library); err != nil {
		return err
	}
	return writeChecksums(archivePath)
}

// writeArchive пишет zip с единственным файлом gigacode.so в корне.
func writeArchive(path string, library []byte) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("создать %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	zipWriter := zip.NewWriter(file)
	header := &zip.FileHeader{
		Name:   libraryName,
		Method: zip.Deflate,
	}
	// SetMode хранит права в расширенных полях заголовка; магазин
	// восстанавливает их при установке (FileInfo().Mode().Perm()).
	header.SetMode(libraryMode)
	if err := writeEntry(zipWriter, header, library); err != nil {
		return fmt.Errorf("записать %s: %w", path, err)
	}
	if err := zipWriter.Close(); err != nil {
		return fmt.Errorf("завершить %s: %w", path, err)
	}
	return file.Close()
}

// writeEntry добавляет в архив одну запись с содержимым library.
func writeEntry(zipWriter *zip.Writer, header *zip.FileHeader, library []byte) error {
	entry, err := zipWriter.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("создать запись %s: %w", header.Name, err)
	}
	_, err = entry.Write(library)
	return err
}

// writeChecksums пишет checksums.txt рядом с архивом: "<sha256>  <имя>".
// Два пробела — формат sha256sum, по нему работает `sha256sum -c` и
// проверка магазина плагинов.
func writeChecksums(archivePath string) error {
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return fmt.Errorf("прочитать %s: %w", archivePath, err)
	}
	sum := sha256.Sum256(data)
	line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(archivePath) + "\n"

	checksumPath := filepath.Join(filepath.Dir(archivePath), "checksums.txt")
	if err := os.WriteFile(checksumPath, []byte(line), 0o644); err != nil {
		return fmt.Errorf("записать %s: %w", checksumPath, err)
	}
	fmt.Println("готово:", archivePath)
	fmt.Println("готово:", checksumPath)
	return nil
}
