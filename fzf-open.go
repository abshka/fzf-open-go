package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	// "syscall" // Обычно не нужен для Start()
	//"time" // Можно раскомментировать для time.Sleep при отладке
)

// ========================================================================
//                          CONFIGURATION SECTION
// ========================================================================
//   Пользователь редактирует значения в этом разделе перед компиляцией
// ========================================================================

// --- Основные настройки ---
const (
	defaultTerminal    = "alacritty"    // Терминал для запуска fzf (если используется флаг -n)
	defaultStartingDir = "~"            // Стартовая директория для fzf (можно использовать ~ или $HOME)
	winTitleFlag       = "--title"      // Флаг терминала для установки заголовка окна
	winTitle           = "fzf-open-run" // Заголовок окна терминала для fzf
)

// --- Ассоциации приложений ---
const (
	// Приложения для конкретных типов файлов
	textEditor        = "zeditor"         // Для текстовых файлов, кода, конфигов и т.д.
	pdfViewer         = "zathura"         // Для PDF документов
	imageViewer       = "eog"             // Для изображений (png, jpg, gif, и т.д.)
	videoPlayer       = "vlc"             // Для видео и аудио файлов
	spreadsheetEditor = "wps"             // Для CSV, ODS, XLSX файлов
	webBrowser        = "thorium-browser" // Для HTML файлов
	docxViewer        = "wps"             // Для Word документов (docx, doc)
	//archiveManager    = "file-roller"     // Для архивов (zip, tar, gz, и т.д.)
	// --- Fallback Opener ---
	// Используется, если ни одно правило не подошло.
	// 'xdg-open' - стандартный обработчик freedesktop.org.
	fallbackOpener = "xdg-open"
)

// --- Временный файл для вывода fzf ---
const tmpFzfOutput = "/tmp/fzf-open"

// ========================================================================
//                        END OF CONFIGURATION SECTION
// ========================================================================

// Config структура для хранения *операционных* настроек (флаги и т.д.)
type Config struct {
	Terminal    string
	StartingDir string
	SpawnTerm   bool // Управляется флагом -n
	ForceTermUI bool // Принудительно использовать TUI-режим в терминале
	NoAutoClose bool // Флаг для предотвращения автоматического закрытия
}

func main() {
	// --- 0. Настройка логгера (опционально, для лучшей диагностики) ---
	log.SetFlags(log.Lshortfile) // Показывать имя файла и строку в логах

	// --- 1. Инициализация операционной конфигурации (дефолты + флаги) ---
	cfg := initializeAndParseFlags()

	// --- 2. Расшифровка стартовой директории (~, $VARS) ---
	var err error
	cfg.StartingDir, err = expandPath(cfg.StartingDir)
	if err != nil {
		log.Printf("Error expanding Starting Directory path '%s': %v", cfg.StartingDir, err) // Используем log.Printf
		os.Exit(1)
	}
	log.Printf("Starting directory resolved to: %s", cfg.StartingDir)

	// Проверяем, запущены ли мы из терминала
	isTerminal := isRunningInTerminal()
	log.Printf("Running in terminal: %t", isTerminal)

	// --- 3. Получение пути через fzf ---
	log.Println("Getting path via FZF...")
	selectedPath, err := getPathViaFZF(cfg, isTerminal)
	if err != nil {
		// Ошибки fzf (включая отмену) уже обработаны внутри getPathViaFZF
		log.Println("getPathViaFZF returned an error or user cancelled.")
		waitForUserIfNoAutoClose(cfg)
		os.Exit(0) // Просто выходим, если путь не выбран или ошибка обработана
	}

	if selectedPath == "" {
		log.Println("No path selected from FZF.")
		waitForUserIfNoAutoClose(cfg)
		os.Exit(0)
	}
	log.Printf("Path selected: %s", selectedPath)

	// --- 4. Открытие выбранного файла с использованием встроенной логики ---
	log.Printf("Attempting to open: %s", selectedPath)
	err = openFileWithConfiguredApp(selectedPath)
	if err != nil {
		// Ошибка уже выведена в openFileWithConfiguredApp или launchApp
		log.Printf("Failed to open file: %v", err)
		// Если скрипт не должен автоматически закрываться, ждем ввода
		waitForUserIfNoAutoClose(cfg)
		os.Exit(1) // Выходим с кодом ошибки
	}

	log.Println("File opening process initiated successfully.")

	// Если скрипт не должен автоматически закрываться, ждем ввода
	waitForUserIfNoAutoClose(cfg)
}

// waitForUserIfNoAutoClose ожидает ввода пользователя если установлен флаг NoAutoClose
func waitForUserIfNoAutoClose(cfg *Config) {
	if cfg.NoAutoClose {
		fmt.Println("\nНажмите Enter для выхода...")
		fmt.Scanln() // Ждем ввода пользователя
	}
}

// isRunningInTerminal проверяет, запущена ли программа в терминале
func isRunningInTerminal() bool {
	// Проверяем, является ли stdin терминалом
	fileInfo, _ := os.Stdin.Stat()
	return (fileInfo.Mode() & os.ModeCharDevice) != 0
}

// initializeAndParseFlags устанавливает дефолты и читает флаги
func initializeAndParseFlags() *Config {
	cfg := &Config{
		Terminal:    defaultTerminal,
		StartingDir: defaultStartingDir,
		SpawnTerm:   false, // Дефолт - не запускать новый терминал
		ForceTermUI: false, // Дефолт - автоматическое определение
		NoAutoClose: false, // Дефолт - автоматически закрывать окно
	}

	// Определяем флаги, используя указатели на поля структуры Config
	flag.BoolVar(&cfg.SpawnTerm, "n", cfg.SpawnTerm, "Spawn fzf in a new terminal window")
	flag.StringVar(&cfg.StartingDir, "d", cfg.StartingDir, "Starting directory for fzf")
	flag.StringVar(&cfg.Terminal, "t", cfg.Terminal, "Terminal emulator command")
	flag.BoolVar(&cfg.ForceTermUI, "f", cfg.ForceTermUI, "Force terminal UI mode even when running outside terminal")
	flag.BoolVar(&cfg.NoAutoClose, "k", cfg.NoAutoClose, "Keep window open (don't auto-close)")

	flag.Parse()
	return cfg
}

// expandPath обрабатывает ~ и $VARS в пути
func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "~") {
		currentUser, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("could not get current user for ~ expansion: %w", err)
		}
		path = filepath.Join(currentUser.HomeDir, path[1:])
	}
	return os.ExpandEnv(path), nil
}

// getPathViaFZF запускает fzf и возвращает выбранный абсолютный путь
func getPathViaFZF(cfg *Config, isTerminal bool) (string, error) {
	// --- Проверка StartingDir ---
	if info, err := os.Stat(cfg.StartingDir); err != nil || !info.IsDir() {
		originalDir := cfg.StartingDir
		fallbackDir, _ := expandPath("~") // Получаем домашнюю директорию
		cfg.StartingDir = fallbackDir
		fmt.Fprintf(os.Stderr, "Warning: STARTING_DIR %q is invalid, falling back to %q\n", originalDir, cfg.StartingDir)
		log.Printf("Warning: STARTING_DIR %q is invalid, falling back to %q", originalDir, cfg.StartingDir) // Лог
		if infoFallback, errFallback := os.Stat(cfg.StartingDir); errFallback != nil || !infoFallback.IsDir() {
			err := fmt.Errorf("fallback STARTING_DIR %q is also invalid", cfg.StartingDir)
			log.Print(err)
			return "", err
		}
	}

	// --- Формирование команды fzf ---
	// ИЗМЕНЕНО: Используем две команды find для приоритезации
	// Сначала файлы в текущей директории (-maxdepth 1), потом в поддиректориях (-mindepth 2)
	// Используем %q для безопасной вставки путей в команду оболочки
	fzfCommand := fmt.Sprintf(
		`cd %q; (find . -maxdepth 1 -type f -not -path './proc/*' -not -path './sys/*' -not -path './dev/*' ; find . -mindepth 2 -type f -not -path './proc/*' -not -path './sys/*' -not -path './dev/*') 2>/dev/null | fzf --ansi --prompt='Select file> ' --no-multi > %q`,
		cfg.StartingDir,
		tmpFzfOutput,
	)
	log.Printf("FZF command to be executed: %s", fzfCommand) // Лог команды

	var cmd *exec.Cmd
	var err error

	// Определяем режим запуска:
	// 1. Если мы НЕ в терминале И не требуется force-terminal - запускаем в новом терминале
	// 2. Если мы в терминале ИЛИ требуется force-terminal - запускаем в текущем окружении

	shouldUseNewTerminal := (!isTerminal && !cfg.ForceTermUI) || cfg.SpawnTerm
	log.Printf("Should use new terminal for FZF: %t (isTerminal=%t, ForceTermUI=%t, SpawnTerm=%t)", shouldUseNewTerminal, isTerminal, cfg.ForceTermUI, cfg.SpawnTerm)

	if shouldUseNewTerminal {
		// --- Запуск в новом терминале ---
		log.Printf("Spawning FZF in new terminal: %s", cfg.Terminal)
		args := []string{
			winTitleFlag, // Используем константы
			winTitle,     // Используем константы
			"-e",
			"/bin/sh", "-c",
			fzfCommand,
		}
		cmd = exec.Command(cfg.Terminal, args...)
		// Для Run() нет смысла устанавливать Stdin/out/err, т.к. он ждет завершения
		err = cmd.Run()
	} else {
		// --- Запуск в ТЕКУЩЕЙ среде ---
		log.Println("Running FZF in current environment")
		cmd = exec.Command("/bin/sh", "-c", fzfCommand)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
	}

	// --- Обработка ошибок запуска cmd ---
	if err != nil {
		log.Printf("FZF command execution failed: %v", err)
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Код 130 обычно означает Ctrl+C (отмена пользователем) в fzf
			// Код 1 может означать, что ничего не выбрано или другая ошибка fzf
			if exitErr.ExitCode() == 130 {
				log.Println("FZF cancelled by user (exit code 130).")
				return "", nil // Отмена - не ошибка приложения
			}
			log.Printf("FZF exited with non-zero code: %d", exitErr.ExitCode())
			// Другие ненулевые коды выхода также считаем не фатальными для fzf-open
			return "", nil
		}
		// Ошибка запуска самой команды (sh, cd, find, fzf не найдены и т.д.)
		fmt.Fprintf(os.Stderr, "Error executing fzf command: %v\n", err)
		return "", err // Это уже серьезная ошибка
	}

	// --- Чтение результата из файла ---
	defer os.Remove(tmpFzfOutput) // Удаляем файл после использования
	content, err := os.ReadFile(tmpFzfOutput)
	if err != nil {
		log.Printf("Error reading fzf output file %q: %v", tmpFzfOutput, err)
		if errors.Is(err, os.ErrNotExist) { // Файл не создан (вероятно, fzf был отменен или ничего не выбрал)
			log.Println("FZF output file does not exist.")
			return "", nil
		}
		// Другая ошибка чтения файла
		fmt.Fprintf(os.Stderr, "Error reading fzf output file %q: %v\n", tmpFzfOutput, err)
		return "", nil // Считаем это не фатальным, просто ничего не выбрано
	}

	selectedRelativePath := strings.TrimSpace(string(content))
	if selectedRelativePath == "" {
		log.Println("FZF output file was empty.")
		return "", nil // Ничего не выбрано
	}
	log.Printf("Relative path from FZF: %s", selectedRelativePath)

	absolutePath := filepath.Join(cfg.StartingDir, selectedRelativePath)
	absolutePath, err = filepath.Abs(absolutePath) // Делаем путь абсолютным и каноническим
	if err != nil {
		log.Printf("Error making path absolute %q: %v", absolutePath, err)
		fmt.Fprintf(os.Stderr, "Error resolving path %q: %v\n", absolutePath, err)
		return "", nil // Не можем сформировать путь
	}
	log.Printf("Absolute path constructed: %s", absolutePath)

	// Проверяем существование финального пути
	if _, err := os.Stat(absolutePath); err != nil {
		// Ошибка может быть из-за неверного пути или прав доступа
		log.Printf("Warning: Constructed path does not exist or is inaccessible: %q (%v)", absolutePath, err)
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist or is inaccessible: %q (%v)\n", absolutePath, err)
		return "", nil // Нечего открывать
	}

	return absolutePath, nil
}

// openFileWithConfiguredApp - основная логика выбора приложения
func openFileWithConfiguredApp(filePath string) error {
	log.Printf("Determining application for: %s", filePath)
	// --- Проверка существования файла (дополнительная) ---
	if _, err := os.Stat(filePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: File or directory not found: %q (%v)\n", filePath, err)
		log.Printf("Error: File or directory not found before opening: %q (%v)", filePath, err)
		return err // Возвращаем ошибку
	}

	// --- Сбор информации о файле ---
	fileName := filepath.Base(filePath)
	extWithDot := filepath.Ext(fileName)
	extLower := strings.ToLower(strings.TrimPrefix(extWithDot, "."))

	// Особая обработка для файлов типа ".bashrc" (скрытый без расширения)
	if strings.HasPrefix(fileName, ".") && extWithDot == "" {
		extLower = "" // Считаем, что расширения нет
	}
	log.Printf("File: %q, Extension (lower): %q", fileName, extLower)

	// --- Получение MIME типа ---
	mimeType := getMimeType(filePath) // Используем хелпер
	log.Printf("MIME Type: %q", mimeType)

	// --- Логика выбора приложения ---

	var appToLaunch string // Переменная для хранения выбранного приложения
	var appLaunched bool

	// 1. Проверка по расширению
	switch extLower {
	case "pdf":
		appToLaunch = pdfViewer
	case "docx", "doc":
		appToLaunch = docxViewer
	case "png", "jpg", "jpeg", "gif", "bmp", "webp", "svg", "ico", "tif", "tiff":
		appToLaunch = imageViewer
	case "flv", "avi", "mov", "mp4", "mkv", "webm", "wmv", "mpeg", "mpg", "mp3", "ogg", "oga", "wav", "flac", "opus", "aac", "m4a":
		appToLaunch = videoPlayer
	case "csv", "tsv", "ods", "xlsx":
		appToLaunch = spreadsheetEditor
	case "htm", "html", "xhtml":
		appToLaunch = webBrowser
	case "txt", "md", "markdown", "sh", "bash", "zsh", "fish", "py", "rb", "js", "jsx", "ts", "tsx", "c", "cpp", "h", "hpp", "java", "go", "rs", "php", "pl", "lua", "sql", "json", "yaml", "yml", "toml", "xml", "css", "scss", "less", "conf", "cfg", "log", "ini", "desktop", "service", "env", "gitignore", "dockerfile", "":
		if extLower == "" || strings.HasPrefix(mimeType, "text/") || mimeType == "application/x-shellscript" || mimeType == "application/javascript" || mimeType == "application/json" || mimeType == "application/xml" || mimeType == "inode/x-empty" || mimeType == "" { // Добавлено inode/x-empty и пустой MIME для текстовых
			appToLaunch = textEditor
		} else {
			log.Printf("Extension %q matched text rule, but MIME %q is not text-like. Will proceed to MIME check.", extLower, mimeType)
		}
		// case "zip", "tar", "gz", "bz2", "xz", "rar", "7z": appToLaunch = archiveManager
	}

	if appToLaunch != "" {
		log.Printf("Launching based on extension %q: App=%q, File=%q", extLower, appToLaunch, filePath)
		appLaunched = launchApp(appToLaunch, filePath)
		if appLaunched {
			return nil // Успешно запущено по расширению
		}
		// Если запуск не удался, попробуем другие методы
		log.Printf("Launch failed for extension rule. App=%q", appToLaunch)
		appToLaunch = "" // Сбрасываем, чтобы перейти к MIME
	}

	// 2. Проверка по MIME типу (если по расширению не подошло или не сработало)
	if !appLaunched && mimeType != "" {
		log.Printf("Checking MIME type rules for: %q", mimeType)
		switch {
		case strings.HasPrefix(mimeType, "text/"),
			mimeType == "application/x-shellscript",
			mimeType == "application/javascript",
			mimeType == "application/json",
			mimeType == "application/xml",
			mimeType == "inode/x-empty": // Пустые файлы часто определяются так
			appToLaunch = textEditor
		case strings.HasPrefix(mimeType, "image/"):
			appToLaunch = imageViewer
		case strings.HasPrefix(mimeType, "video/"), strings.HasPrefix(mimeType, "audio/"):
			appToLaunch = videoPlayer
		case mimeType == "application/pdf":
			appToLaunch = pdfViewer
		case mimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			mimeType == "application/msword",
			mimeType == "application/vnd.oasis.opendocument.text":
			appToLaunch = docxViewer
		case mimeType == "application/vnd.oasis.opendocument.spreadsheet",
			mimeType == "application/vnd.ms-excel",
			mimeType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
			appToLaunch = spreadsheetEditor
			// case mimeType == "application/zip", ... : appToLaunch = archiveManager
			// case mimeType == "inode/directory": appToLaunch = fallbackOpener // Можно добавить правило для директорий
		}

		if appToLaunch != "" {
			log.Printf("Launching based on MIME %q: App=%q, File=%q", mimeType, appToLaunch, filePath)
			appLaunched = launchApp(appToLaunch, filePath)
			if appLaunched {
				return nil // Успешно запущено по MIME
			}
			log.Printf("Launch failed for MIME rule. App=%q", appToLaunch)
		}
	}

	// 3. Fallback (если ничего не подошло или запуск не удался)
	if !appLaunched {
		appToLaunch = fallbackOpener
		log.Printf("No specific rule matched or previous launches failed. Falling back to: App=%q, File=%q", appToLaunch, filePath)
		fmt.Fprintf(os.Stderr, "Info: No specific rule matched for %q (MIME: %q). Falling back to %q...\n", fileName, mimeType, fallbackOpener) // Инфо для пользователя
		appLaunched = launchApp(appToLaunch, filePath)
		if !appLaunched {
			// Если даже fallback не запустился
			err := fmt.Errorf("fallback opener %q failed to launch for %q", fallbackOpener, filePath)
			log.Print(err)
			return err
		}
	}

	return nil // Успешно запущен fallback или одна из предыдущих попыток
}

// getMimeType получает MIME тип файла с помощью xdg-mime
func getMimeType(filePath string) string {
	// Проверяем, существует ли xdg-mime
	xdgMimePath, err := exec.LookPath("xdg-mime")
	if err != nil {
		log.Println("Warning: 'xdg-mime' command not found, cannot determine MIME type.") // Лог
		// fmt.Fprintln(os.Stderr, "Warning: 'xdg-mime' command not found, cannot determine MIME type.") // Можно оставить для пользователя
		return "" // Не можем определить MIME
	}

	// Запускаем xdg-mime query filetype <path>
	cmd := exec.Command(xdgMimePath, "query", "filetype", filePath)
	output, err := cmd.Output() // Output() запускает и ждет завершения

	if err != nil {
		// Ошибка может быть, если файл не найден или xdg-mime не может определить тип
		log.Printf("Warning: xdg-mime failed for %q: %v", filePath, err) // Лог
		// fmt.Fprintf(os.Stderr, "Warning: xdg-mime failed for %q: %v\n", filePath, err) // Можно оставить для пользователя
		return ""
	}

	return strings.TrimSpace(string(output))
}

// launchApp запускает приложение в фоне
func launchApp(appCommand string, filePath string) bool {
	// Разбиваем команду на части (приложение + аргументы, если есть)
	// Пример: "libreoffice --writer" -> ["libreoffice", "--writer"]
	parts := strings.Fields(appCommand)
	if len(parts) == 0 {
		log.Printf("Error: Empty application command provided.")
		fmt.Fprintf(os.Stderr, "Error: Invalid empty application command.\n")
		return false
	}
	appName := parts[0]
	appArgs := parts[1:]

	// Проверяем, существует ли исполняемый файл в PATH
	appPath, err := exec.LookPath(appName)
	if err != nil {
		log.Printf("Error: Application command not found in PATH: %q", appName)
		fmt.Fprintf(os.Stderr, "Error: Application command not found in PATH: %q\n", appName)
		return false
	}
	log.Printf("Found application %q at %q", appName, appPath)

	// Собираем финальные аргументы: аргументы_из_команды + путь_к_файлу
	finalArgs := append(appArgs, filePath)
	log.Printf("Launching: %s %v", appPath, finalArgs) // Логгируем полный вызов

	cmd := exec.Command(appPath, finalArgs...)

	// Start() запускает команду и не ждет ее завершения (аналог &)
	// Мы НЕ хотим перенаправлять Stdin/out/err сюда, они должны наследоваться
	// или обрабатываться самой запускаемой программой.
	err = cmd.Start()
	if err != nil {
		log.Printf("Error starting application %q for file %q: %v", appCommand, filePath, err)
		fmt.Fprintf(os.Stderr, "Error starting application %q for file %q: %v\n", appCommand, filePath, err)
		return false
	}

	// Важно: Мы не ждем завершения cmd.Wait().
	// Процесс запущен в фоне. Сразу возвращаем true.
	// cmd.Process.Release() // Освобождаем ресурсы, связанные с дочерним процессом в Go
	// Вызов Release() не обязателен, но может быть хорошей практикой,
	// если родительский процесс fzf-open живет долго (здесь он обычно завершается).
	// Пока оставим без Release() для простоты.

	log.Printf("Application %q launched successfully in background (PID: %d)", appCommand, cmd.Process.Pid)
	return true
}
