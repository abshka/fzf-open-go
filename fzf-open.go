package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ========================================================================
//                          CONFIGURATION SECTION
// ========================================================================
//   Пользователь редактирует значения в этом разделе перед компиляцией
// ========================================================================

// DefaultConfig содержит конфигурационные константы
type DefaultConfig struct {
	Terminal     string // Терминал для запуска fzf
	StartingDir  string // Стартовая директория для fzf
	WinTitleFlag string // Флаг терминала для установки заголовка окна
	WinTitle     string // Заголовок окна терминала для fzf
}

// AppAssociations содержит ассоциации приложений с типами файлов
type AppAssociations struct {
	TextEditor        string
	PDFViewer         string
	ImageViewer       string
	VideoPlayer       string
	SpreadsheetEditor string
	WebBrowser        string
	DocxViewer        string
	FallbackOpener    string
}

// Используем константы для часто используемых MIME типов
const (
	mimeTextPrefix        = "text/"
	mimeApplicationScript = "application/x-shellscript"
	mimeApplicationJS     = "application/javascript"
	mimeApplicationJSON   = "application/json"
	mimeApplicationXML    = "application/xml"
	mimeInodeEmpty        = "inode/x-empty"
	mimeImagePrefix       = "image/"
	mimeVideoPrefix       = "video/"
	mimeAudioPrefix       = "audio/"
	mimePDF               = "application/pdf"
	mimeWordDocx          = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeWordDoc           = "application/msword"
	mimeODT               = "application/vnd.oasis.opendocument.text"
	mimeODS               = "application/vnd.oasis.opendocument.spreadsheet"
	mimeExcel             = "application/vnd.ms-excel"
	mimeExcelX            = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

var (
	// Основные настройки
	defaultConfig = DefaultConfig{
		Terminal:     "alacritty",
		StartingDir:  "~",
		WinTitleFlag: "--title",
		WinTitle:     "fzf-open-run",
	}

	// Ассоциации приложений
	appAssociations = AppAssociations{
		TextEditor:        "zeditor",
		PDFViewer:         "zathura",
		ImageViewer:       "eog",
		VideoPlayer:       "vlc",
		SpreadsheetEditor: "wps",
		WebBrowser:        "thorium-browser",
		DocxViewer:        "wps",
		FallbackOpener:    "xdg-open",
	}

	// Временный файл для вывода fzf
	tmpFzfOutput = "/tmp/fzf-open"

	// Кэш для результатов LookPath
	pathCache     = make(map[string]string)
	pathCacheLock sync.RWMutex
)

// ========================================================================
//                        END OF CONFIGURATION SECTION
// ========================================================================

// Config структура для хранения операционных настроек (флаги и т.д.)
type Config struct {
	Terminal    string
	StartingDir string
	SpawnTerm   bool // Управляется флагом -n
	NoAutoClose bool // Флаг для предотвращения автоматического закрытия
}

func main() {
	// Инициализация операционной конфигурации
	cfg := initializeAndParseFlags()

	// Расшифровка стартовой директории
	startingDir, err := expandPath(cfg.StartingDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error expanding Starting Directory path '%s': %v\n", cfg.StartingDir, err)
		os.Exit(1)
	}
	cfg.StartingDir = startingDir

	// Получение пути через fzf
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	selectedPath, err := getPathViaFZF(ctx, cfg)
	if err != nil {
		waitForUserIfNoAutoClose(cfg)
		os.Exit(0)
	}

	if selectedPath == "" {
		waitForUserIfNoAutoClose(cfg)
		os.Exit(0)
	}

	// Открытие выбранного файла
	if err := openFileWithConfiguredApp(selectedPath); err != nil {
		waitForUserIfNoAutoClose(cfg)
		os.Exit(1)
	}

	waitForUserIfNoAutoClose(cfg)
}

// waitForUserIfNoAutoClose ожидает ввода пользователя если установлен флаг NoAutoClose
func waitForUserIfNoAutoClose(cfg *Config) {
	if cfg.NoAutoClose {
		fmt.Println("\nНажмите Enter для выхода...")
		fmt.Scanln()
	}
}

// initializeAndParseFlags устанавливает дефолты и читает флаги
func initializeAndParseFlags() *Config {
	cfg := &Config{
		Terminal:    defaultConfig.Terminal,
		StartingDir: defaultConfig.StartingDir,
		SpawnTerm:   false,
		NoAutoClose: false,
	}

	flag.BoolVar(&cfg.SpawnTerm, "n", cfg.SpawnTerm, "Spawn fzf in a new terminal window")
	flag.StringVar(&cfg.StartingDir, "d", cfg.StartingDir, "Starting directory for fzf")
	flag.StringVar(&cfg.Terminal, "t", cfg.Terminal, "Terminal emulator command")
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
func getPathViaFZF(ctx context.Context, cfg *Config) (string, error) {
	// Проверка StartingDir
	info, err := os.Stat(cfg.StartingDir)
	if err != nil || !info.IsDir() {
		originalDir := cfg.StartingDir
		fallbackDir, _ := expandPath("~")
		cfg.StartingDir = fallbackDir
		fmt.Fprintf(os.Stderr, "Warning: STARTING_DIR %q is invalid, falling back to %q\n", originalDir, cfg.StartingDir)

		infoFallback, errFallback := os.Stat(cfg.StartingDir)
		if errFallback != nil || !infoFallback.IsDir() {
			return "", fmt.Errorf("fallback STARTING_DIR %q is also invalid", cfg.StartingDir)
		}
	}

	// Формирование команды fzf - используем стандартный fzf без предварительной фильтрации
	fzfCommand := fmt.Sprintf(
		`cd %q && fzf --ansi --prompt='Select file> ' --no-multi > %q`,
		cfg.StartingDir,
		tmpFzfOutput,
	)

	var cmd *exec.Cmd

	if cfg.SpawnTerm {
		// Запуск в новом терминале
		args := []string{
			defaultConfig.WinTitleFlag,
			defaultConfig.WinTitle,
			"-e",
			"/bin/sh", "-c",
			fzfCommand,
		}
		cmd = exec.CommandContext(ctx, cfg.Terminal, args...)
		err = cmd.Run()
	} else {
		// Запуск в текущей среде
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", fzfCommand)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
	}

	// Обработка ошибок запуска cmd
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 130 {
				return "", nil
			}
			return "", nil
		}
		fmt.Fprintf(os.Stderr, "Error executing fzf command: %v\n", err)
		return "", err
	}

	// Чтение результата из файла
	defer os.Remove(tmpFzfOutput)
	content, err := os.ReadFile(tmpFzfOutput)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		fmt.Fprintf(os.Stderr, "Error reading fzf output file %q: %v\n", tmpFzfOutput, err)
		return "", nil
	}

	selectedRelativePath := strings.TrimSpace(string(content))
	if selectedRelativePath == "" {
		return "", nil
	}

	// Преобразование в абсолютный путь
	absolutePath := filepath.Join(cfg.StartingDir, selectedRelativePath)
	absolutePath, err = filepath.Abs(absolutePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path %q: %v\n", absolutePath, err)
		return "", nil
	}

	// Проверка существования пути
	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist or is inaccessible: %q (%v)\n", absolutePath, err)
		return "", nil
	}

	return absolutePath, nil
}

// FileTypeInfo содержит информацию о типе файла
type FileTypeInfo struct {
	Path     string
	FileName string
	Ext      string
	MIMEType string
}

// openFileWithConfiguredApp - основная логика выбора приложения
func openFileWithConfiguredApp(filePath string) error {
	// Проверка существования файла
	if _, err := os.Stat(filePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: File or directory not found: %q (%v)\n", filePath, err)
		return err
	}

	// Собираем информацию о файле
	fileInfo := getFileTypeInfo(filePath)

	// Выбор и запуск приложения только один раз через определение приложения
	// Используем только одно из определений - по расширению или по MIME типу, но не оба
	var appToLaunch string

	// Сначала пробуем по расширению
	appToLaunch = getAppByExtension(fileInfo.Ext, fileInfo.MIMEType)

	// Если по расширению не нашли, пробуем по MIME
	if appToLaunch == "" && fileInfo.MIMEType != "" {
		appToLaunch = getAppByMIME(fileInfo.MIMEType)
	}

	// Запускаем приложение, если его удалось определить
	if appToLaunch != "" {
		if launchApp(appToLaunch, filePath) {
			return nil
		}
	}

	// Fallback если не определили приложение или не смогли запустить
	fmt.Fprintf(os.Stderr, "Info: No specific rule matched for %q (MIME: %q). Falling back to %q...\n",
		fileInfo.FileName, fileInfo.MIMEType, appAssociations.FallbackOpener)

	if !launchApp(appAssociations.FallbackOpener, filePath) {
		return fmt.Errorf("fallback opener %q failed to launch for %q", appAssociations.FallbackOpener, filePath)
	}

	return nil
}

// getFileTypeInfo собирает информацию о файле
func getFileTypeInfo(filePath string) FileTypeInfo {
	fileName := filepath.Base(filePath)
	extWithDot := filepath.Ext(fileName)
	extLower := strings.ToLower(strings.TrimPrefix(extWithDot, "."))

	// Особая обработка для файлов типа ".bashrc" (скрытый без расширения)
	if strings.HasPrefix(fileName, ".") && extWithDot == "" {
		extLower = ""
	}

	// Получаем MIME тип только если это не директория
	var mimeType string
	if fi, err := os.Stat(filePath); err == nil && !fi.IsDir() {
		mimeType = getMimeType(filePath)
	}

	return FileTypeInfo{
		Path:     filePath,
		FileName: fileName,
		Ext:      extLower,
		MIMEType: mimeType,
	}
}

// getAppByExtension определяет приложение по расширению файла
func getAppByExtension(ext, mimeType string) string {
	switch ext {
	case "pdf":
		return appAssociations.PDFViewer
	case "docx", "doc":
		return appAssociations.DocxViewer
	case "png", "jpg", "jpeg", "gif", "bmp", "webp", "svg", "ico", "tif", "tiff":
		return appAssociations.ImageViewer
	case "flv", "avi", "mov", "mp4", "mkv", "webm", "wmv", "mpeg", "mpg", "mp3", "ogg", "oga", "wav", "flac", "opus", "aac", "m4a":
		return appAssociations.VideoPlayer
	case "csv", "tsv", "ods", "xlsx":
		return appAssociations.SpreadsheetEditor
	case "htm", "html", "xhtml":
		return appAssociations.WebBrowser
	case "txt", "md", "markdown", "sh", "bash", "zsh", "fish", "py", "rb", "js", "jsx", "ts", "tsx", "c", "cpp", "h", "hpp", "java", "go", "rs", "php", "pl", "lua", "sql", "json", "yaml", "yml", "toml", "xml", "css", "scss", "less", "conf", "cfg", "log", "ini", "desktop", "service", "env", "gitignore", "dockerfile", "":
		if ext == "" || strings.HasPrefix(mimeType, mimeTextPrefix) ||
			mimeType == mimeApplicationScript ||
			mimeType == mimeApplicationJS ||
			mimeType == mimeApplicationJSON ||
			mimeType == mimeApplicationXML ||
			mimeType == mimeInodeEmpty ||
			mimeType == "" {
			return appAssociations.TextEditor
		}
	}
	return ""
}

// getAppByMIME определяет приложение по MIME типу
func getAppByMIME(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, mimeTextPrefix),
		mimeType == mimeApplicationScript,
		mimeType == mimeApplicationJS,
		mimeType == mimeApplicationJSON,
		mimeType == mimeApplicationXML,
		mimeType == mimeInodeEmpty:
		return appAssociations.TextEditor
	case strings.HasPrefix(mimeType, mimeImagePrefix):
		return appAssociations.ImageViewer
	case strings.HasPrefix(mimeType, mimeVideoPrefix), strings.HasPrefix(mimeType, mimeAudioPrefix):
		return appAssociations.VideoPlayer
	case mimeType == mimePDF:
		return appAssociations.PDFViewer
	case mimeType == mimeWordDocx,
		mimeType == mimeWordDoc,
		mimeType == mimeODT:
		return appAssociations.DocxViewer
	case mimeType == mimeODS,
		mimeType == mimeExcel,
		mimeType == mimeExcelX:
		return appAssociations.SpreadsheetEditor
	}
	return ""
}

// getMimeType получает MIME тип файла с помощью xdg-mime
func getMimeType(filePath string) string {
	xdgMimePath, err := cachedLookPath("xdg-mime")
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, xdgMimePath, "query", "filetype", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(output))
}

// cachedLookPath кэширует результаты exec.LookPath для улучшения производительности
func cachedLookPath(name string) (string, error) {
	pathCacheLock.RLock()
	path, ok := pathCache[name]
	pathCacheLock.RUnlock()

	if ok {
		return path, nil
	}

	// Если не найдено в кэше, ищем и добавляем
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}

	pathCacheLock.Lock()
	pathCache[name] = path
	pathCacheLock.Unlock()

	return path, nil
}

// launchApp запускает приложение в фоне
func launchApp(appCommand string, filePath string) bool {
	parts := strings.Fields(appCommand)
	if len(parts) == 0 {
		fmt.Fprintf(os.Stderr, "Error: Invalid empty application command.\n")
		return false
	}

	appName := parts[0]
	appArgs := parts[1:]

	appPath, err := cachedLookPath(appName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Application command not found in PATH: %q\n", appName)
		return false
	}

	finalArgs := append(appArgs, filePath)
	cmd := exec.Command(appPath, finalArgs...)

	// Установка группы процесса для предотвращения наследования сигналов
	// Отключаем привязку к родительскому процессу
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting application %q for file %q: %v\n", appCommand, filePath, err)
		return false
	}

	return true
}
