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

// DefaultConfig содержит конфигурационные константы
type DefaultConfig struct {
	Terminal     string
	StartingDir  string
	WinTitleFlag string
	WinTitle     string
	FzfCommand   string
	ShellToUse   string
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

// Константы для часто используемых MIME типов
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
	defaultConfig = DefaultConfig{
		Terminal:     "alacritty",
		StartingDir:  "~",
		WinTitleFlag: "--title",
		WinTitle:     "fzf-open-run",
		FzfCommand:   "fzf --ansi --prompt='Select file> ' --no-multi",
		ShellToUse:   "",
	}

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

	tmpFzfOutput = "/tmp/fzf-open"

	pathCache     = make(map[string]string, 10)
	pathCacheLock sync.RWMutex

	userHomeDir string

	textMimePrefixMatch = strings.HasPrefix
)

func init() {
	if u, err := user.Current(); err == nil {
		userHomeDir = u.HomeDir
	}

	detectUserShell()

	go func() {
		commonCommands := []string{"xdg-mime", "sh", "bash", "zsh", "fish", "fzf"}
		for _, cmd := range commonCommands {
			if path, err := exec.LookPath(cmd); err == nil {
				pathCacheLock.Lock()
				pathCache[cmd] = path
				pathCacheLock.Unlock()
			}
		}
	}()
}

// detectUserShell определяет текущую оболочку пользователя и устанавливает ShellToUse
func detectUserShell() {
	shellPath := os.Getenv("SHELL")
	if shellPath != "" {
		shellName := filepath.Base(shellPath)
		if isValidShell(shellName) {
			defaultConfig.ShellToUse = shellName
			return
		}
	}

	possibleShells := []string{"zsh", "bash", "fish", "dash", "sh"}
	for _, shell := range possibleShells {
		if _, err := exec.LookPath(shell); err == nil {
			defaultConfig.ShellToUse = shell
			return
		}
	}

	defaultConfig.ShellToUse = "sh"
	fmt.Fprintf(os.Stderr, "Warning: Could not detect user shell, falling back to /bin/sh\n")
}

// isValidShell проверяет, является ли имя оболочки допустимым
func isValidShell(shellName string) bool {
	validShells := map[string]bool{
		"bash": true,
		"zsh":  true,
		"fish": true,
		"dash": true,
		"sh":   true,
		"ksh":  true,
		"csh":  true,
		"tcsh": true,
	}
	return validShells[shellName]
}

// getShellInteractiveFlag возвращает флаги для интерактивного режима в зависимости от оболочки
func getShellInteractiveFlag(shellName string) []string {
	switch shellName {
	case "fish":
		return []string{"-c"}
	case "zsh", "bash", "sh", "dash", "ksh":
		return []string{"-ic"}
	default:
		return []string{"-c"}
	}
}

// ========================================================================
//                        END OF CONFIGURATION SECTION
// ========================================================================

// Config структура для хранения операционных настроек
type Config struct {
	Terminal    string
	StartingDir string
	SpawnTerm   bool
	NoAutoClose bool
	UseShellIC  bool
}

func main() {
	cfg := initializeAndParseFlags()

	startingDir, err := expandPath(cfg.StartingDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error expanding Starting Directory path '%s': %v\n", cfg.StartingDir, err)
		os.Exit(1)
	}
	cfg.StartingDir = startingDir

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
		UseShellIC:  true,
	}

	flag.BoolVar(&cfg.SpawnTerm, "n", cfg.SpawnTerm, "Spawn fzf in a new terminal window")
	flag.StringVar(&cfg.StartingDir, "d", cfg.StartingDir, "Starting directory for fzf")
	flag.StringVar(&cfg.Terminal, "t", cfg.Terminal, "Terminal emulator command")
	flag.BoolVar(&cfg.NoAutoClose, "k", cfg.NoAutoClose, "Keep window open (don't auto-close)")
	flag.BoolVar(&cfg.UseShellIC, "i", cfg.UseShellIC, "Use interactive shell mode (-ic flags)")

	flag.Parse()
	return cfg
}

// expandPath обрабатывает ~ и $VARS в пути
func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	if strings.HasPrefix(path, "~") {
		if userHomeDir != "" {
			return filepath.Join(userHomeDir, path[1:]), nil
		} else {
			currentUser, err := user.Current()
			if err != nil {
				return "", fmt.Errorf("could not get current user for ~ expansion: %w", err)
			}
			userHomeDir = currentUser.HomeDir
			return filepath.Join(currentUser.HomeDir, path[1:]), nil
		}
	}

	return os.ExpandEnv(path), nil
}

// getPathViaFZF запускает fzf и возвращает выбранный абсолютный путь
func getPathViaFZF(ctx context.Context, cfg *Config) (string, error) {
	info, err := os.Stat(cfg.StartingDir)
	if err != nil || !info.IsDir() {
		originalDir := cfg.StartingDir

		fallbackDir := userHomeDir
		if fallbackDir == "" {
			var err error
			fallbackDir, err = expandPath("~")
			if err != nil {
				return "", fmt.Errorf("failed to determine fallback directory: %w", err)
			}
		}

		cfg.StartingDir = fallbackDir
		fmt.Fprintf(os.Stderr, "Warning: STARTING_DIR %q is invalid, falling back to %q\n", originalDir, cfg.StartingDir)

		infoFallback, errFallback := os.Stat(cfg.StartingDir)
		if errFallback != nil || !infoFallback.IsDir() {
			return "", fmt.Errorf("fallback STARTING_DIR %q is also invalid", cfg.StartingDir)
		}
	}

	fzfCommand := fmt.Sprintf(
		"cd %s && %s > %s",
		shellQuote(cfg.StartingDir),
		defaultConfig.FzfCommand,
		shellQuote(tmpFzfOutput),
	)

	var cmd *exec.Cmd

	if cfg.SpawnTerm {
		var args []string

		if cfg.UseShellIC {
			if defaultConfig.ShellToUse == "" {
				detectUserShell()
			}

			shellInteractiveFlags := getShellInteractiveFlag(defaultConfig.ShellToUse)

			args = []string{
				defaultConfig.WinTitleFlag,
				defaultConfig.WinTitle,
				"-e",
				defaultConfig.ShellToUse,
			}
			args = append(args, shellInteractiveFlags...)
			args = append(args, fzfCommand)
		} else {
			args = []string{
				defaultConfig.WinTitleFlag,
				defaultConfig.WinTitle,
				"-e",
				"/bin/sh", "-c",
				fzfCommand,
			}
		}

		cmd = exec.CommandContext(ctx, cfg.Terminal, args...)
		err = cmd.Run()
	} else {
		var shell string
		var shellArgs []string

		if cfg.UseShellIC && defaultConfig.ShellToUse != "" && defaultConfig.ShellToUse != "sh" {
			shell = defaultConfig.ShellToUse
			shellArgs = getShellInteractiveFlag(defaultConfig.ShellToUse)
			shellArgs = append(shellArgs, fzfCommand)
		} else {
			shell = "/bin/sh"
			shellArgs = []string{"-c", fzfCommand}
		}

		cmd = exec.CommandContext(ctx, shell, shellArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 130 {
			return "", nil
		}
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "Error executing fzf command: %v\n", err)
		}
		return "", nil
	}

	content, err := os.ReadFile(tmpFzfOutput)
	defer os.Remove(tmpFzfOutput)

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

	absolutePath := filepath.Join(cfg.StartingDir, selectedRelativePath)
	absolutePath, err = filepath.Abs(absolutePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path %q: %v\n", absolutePath, err)
		return "", nil
	}

	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist or is inaccessible: %q (%v)\n", absolutePath, err)
		return "", nil
	}

	return absolutePath, nil
}

// shellQuote заключает строку в кавычки для использования в shell командах
func shellQuote(s string) string {
	return "\"" + strings.Replace(s, "\"", "\\\"", -1) + "\""
}

// FileTypeInfo содержит информацию о типе файла
type FileTypeInfo struct {
	Path     string
	FileName string
	Ext      string
	MIMEType string
}

// Карты для быстрого поиска по расширению
var (
	extToPDFViewer   = map[string]bool{"pdf": true}
	extToDocxViewer  = map[string]bool{"docx": true, "doc": true}
	extToImageViewer = map[string]bool{
		"png": true, "jpg": true, "jpeg": true, "gif": true, "bmp": true,
		"webp": true, "svg": true, "ico": true, "tif": true, "tiff": true,
	}
	extToVideoPlayer = map[string]bool{
		"flv": true, "avi": true, "mov": true, "mp4": true, "mkv": true, "webm": true,
		"wmv": true, "mpeg": true, "mpg": true, "mp3": true, "ogg": true, "oga": true,
		"wav": true, "flac": true, "opus": true, "aac": true, "m4a": true,
	}
	extToSpreadsheet = map[string]bool{"csv": true, "tsv": true, "ods": true, "xlsx": true}
	extToWebBrowser  = map[string]bool{"htm": true, "html": true, "xhtml": true}
	extToTextEditor  = map[string]bool{
		"txt": true, "md": true, "markdown": true, "sh": true, "bash": true, "zsh": true,
		"fish": true, "py": true, "rb": true, "js": true, "jsx": true, "ts": true, "tsx": true,
		"c": true, "cpp": true, "h": true, "hpp": true, "java": true, "go": true, "rs": true,
		"php": true, "pl": true, "lua": true, "sql": true, "json": true, "yaml": true, "yml": true,
		"toml": true, "xml": true, "css": true, "scss": true, "less": true, "conf": true,
		"cfg": true, "log": true, "ini": true, "desktop": true, "service": true, "env": true,
		"gitignore": true, "dockerfile": true, "": true,
	}
)

// openFileWithConfiguredApp - основная логика выбора приложения
func openFileWithConfiguredApp(filePath string) error {
	fi, err := os.Stat(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: File or directory not found: %q (%v)\n", filePath, err)
		return err
	}

	fileInfo := FileTypeInfo{
		FileName: filepath.Base(filePath),
	}

	if fi.IsDir() {
		if launchApp(appAssociations.TextEditor, filePath) {
			return nil
		}
		if launchApp(appAssociations.FallbackOpener, filePath) {
			return nil
		}
		return fmt.Errorf("could not open directory %q with any available application", filePath)
	}

	extWithDot := filepath.Ext(fileInfo.FileName)
	fileInfo.Ext = strings.ToLower(strings.TrimPrefix(extWithDot, "."))

	if strings.HasPrefix(fileInfo.FileName, ".") && extWithDot == "" {
		fileInfo.Ext = ""
	}

	var appToLaunch string
	if extToPDFViewer[fileInfo.Ext] {
		appToLaunch = appAssociations.PDFViewer
	} else if extToDocxViewer[fileInfo.Ext] {
		appToLaunch = appAssociations.DocxViewer
	} else if extToImageViewer[fileInfo.Ext] {
		appToLaunch = appAssociations.ImageViewer
	} else if extToVideoPlayer[fileInfo.Ext] {
		appToLaunch = appAssociations.VideoPlayer
	} else if extToSpreadsheet[fileInfo.Ext] {
		appToLaunch = appAssociations.SpreadsheetEditor
	} else if extToWebBrowser[fileInfo.Ext] {
		appToLaunch = appAssociations.WebBrowser
	} else if extToTextEditor[fileInfo.Ext] {
		if fileInfo.Ext == "" {
			fileInfo.MIMEType = getMimeType(filePath)

			if fileInfo.MIMEType == "" ||
				textMimePrefixMatch(fileInfo.MIMEType, mimeTextPrefix) ||
				fileInfo.MIMEType == mimeApplicationScript ||
				fileInfo.MIMEType == mimeApplicationJS ||
				fileInfo.MIMEType == mimeApplicationJSON ||
				fileInfo.MIMEType == mimeApplicationXML ||
				fileInfo.MIMEType == mimeInodeEmpty {
				appToLaunch = appAssociations.TextEditor
			}
		} else {
			appToLaunch = appAssociations.TextEditor
		}
	}

	if appToLaunch == "" {
		if fileInfo.MIMEType == "" {
			fileInfo.MIMEType = getMimeType(filePath)
		}

		if fileInfo.MIMEType != "" {
			appToLaunch = getAppByMIME(fileInfo.MIMEType)
		}
	}

	if appToLaunch != "" {
		if launchApp(appToLaunch, filePath) {
			return nil
		}
	}

	fmt.Fprintf(os.Stderr, "Info: No specific rule matched for %q (MIME: %q). Falling back to %q...\n",
		fileInfo.FileName, fileInfo.MIMEType, appAssociations.FallbackOpener)

	if !launchApp(appAssociations.FallbackOpener, filePath) {
		return fmt.Errorf("fallback opener %q failed to launch for %q", appAssociations.FallbackOpener, filePath)
	}

	return nil
}

// getFileTypeInfo собирает информацию о файле - оставлено для обратной совместимости
func getFileTypeInfo(filePath string) FileTypeInfo {
	fileName := filepath.Base(filePath)
	extWithDot := filepath.Ext(fileName)
	extLower := strings.ToLower(strings.TrimPrefix(extWithDot, "."))

	if strings.HasPrefix(fileName, ".") && extWithDot == "" {
		extLower = ""
	}

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

// getAppByExtension определяет приложение по расширению файла - оставлено для обратной совместимости
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

// Кэш для MIME типов
var (
	mimeCache     = make(map[string]string, 20)
	mimeCacheLock sync.RWMutex
)

// getMimeType получает MIME тип файла с помощью xdg-mime
func getMimeType(filePath string) string {
	mimeCacheLock.RLock()
	cachedMime, ok := mimeCache[filePath]
	mimeCacheLock.RUnlock()

	if ok {
		return cachedMime
	}

	xdgMimePath, err := cachedLookPath("xdg-mime")
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, xdgMimePath, "query", "filetype", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	mimeType := strings.TrimSpace(string(output))

	mimeCacheLock.Lock()
	mimeCache[filePath] = mimeType
	mimeCacheLock.Unlock()

	return mimeType
}

// cachedLookPath кэширует результаты exec.LookPath
func cachedLookPath(name string) (string, error) {
	pathCacheLock.RLock()
	path, ok := pathCache[name]
	pathCacheLock.RUnlock()

	if ok {
		return path, nil
	}

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

	finalArgs := make([]string, 0, len(appArgs)+1)
	finalArgs = append(finalArgs, appArgs...)
	finalArgs = append(finalArgs, filePath)

	cmd := exec.Command(appPath, finalArgs...)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting application %q for file %q: %v\n", appCommand, filePath, err)
		return false
	}

	return true
}
