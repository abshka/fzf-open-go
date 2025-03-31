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
	"runtime"
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

// MIME типы
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

	pathCache     = make(map[string]string, 32)
	pathCacheLock sync.RWMutex

	userHomeDir string

	textMimePrefixMatch = strings.HasPrefix

	fishFlags         = []string{"-c"}
	shFlags           = []string{"-ic"}
	defaultShellFlags = []string{"-c"}

	validShells = map[string]bool{
		"bash": true,
		"zsh":  true,
		"fish": true,
		"dash": true,
		"sh":   true,
		"ksh":  true,
		"csh":  true,
		"tcsh": true,
	}

	shellDetectOnce sync.Once
	pathCacheOnce   sync.Once
)

func init() {
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)

	if u, err := user.Current(); err == nil {
		userHomeDir = u.HomeDir
	}

	go func() {
		shellDetectOnce.Do(func() {
			detectUserShell()
		})
	}()

	go func() {
		pathCacheOnce.Do(func() {
			commonCommands := []string{"xdg-mime", "sh", "bash", "zsh", "fish", "fzf",
				"zeditor", "zathura", "eog", "vlc", "wps", "thorium-browser", "xdg-open"}

			var wg sync.WaitGroup
			resultChan := make(chan struct {
				cmd  string
				path string
			}, len(commonCommands))

			for _, cmd := range commonCommands {
				wg.Add(1)
				go func(command string) {
					defer wg.Done()
					if path, err := exec.LookPath(command); err == nil {
						resultChan <- struct {
							cmd  string
							path string
						}{command, path}
					}
				}(cmd)
			}

			go func() {
				wg.Wait()
				close(resultChan)
			}()

			for result := range resultChan {
				pathCacheLock.Lock()
				pathCache[result.cmd] = result.path
				pathCacheLock.Unlock()
			}
		})
	}()
}

// detectUserShell определяет текущую оболочку пользователя и устанавливает ShellToUse
func detectUserShell() {
	shellPath := os.Getenv("SHELL")
	if shellPath != "" {
		shellName := filepath.Base(shellPath)
		if validShells[shellName] {
			defaultConfig.ShellToUse = shellName
			return
		}
	}

	possibleShells := []string{"zsh", "bash", "fish", "dash", "sh"}

	resultChan := make(chan string, 1)
	done := make(chan struct{})

	for _, shell := range possibleShells {
		go func(sh string) {
			select {
			case <-done:
				return
			default:
				if _, err := exec.LookPath(sh); err == nil {
					select {
					case resultChan <- sh:
						close(done)
					default:
					}
				}
			}
		}(shell)
	}

	select {
	case shell := <-resultChan:
		defaultConfig.ShellToUse = shell
	case <-time.After(200 * time.Millisecond):
		defaultConfig.ShellToUse = "sh"
		fmt.Fprintf(os.Stderr, "Warning: Could not detect user shell, falling back to /bin/sh\n")
	}
}

// getShellInteractiveFlag возвращает флаги для интерактивного режима в зависимости от оболочки
func getShellInteractiveFlag(shellName string) []string {
	switch shellName {
	case "fish":
		return fishFlags
	case "zsh", "bash", "sh", "dash", "ksh":
		return shFlags
	default:
		return defaultShellFlags
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	if path[0] == '~' {
		if userHomeDir != "" {
			if len(path) == 1 {
				return userHomeDir, nil
			}
			var sb strings.Builder
			sb.Grow(len(userHomeDir) + len(path) - 1)
			sb.WriteString(userHomeDir)
			sb.WriteString(path[1:])
			return sb.String(), nil
		} else {
			currentUser, err := user.Current()
			if err != nil {
				return "", fmt.Errorf("could not get current user for ~ expansion: %w", err)
			}
			userHomeDir = currentUser.HomeDir
			if len(path) == 1 {
				return userHomeDir, nil
			}
			return filepath.Join(userHomeDir, path[1:]), nil
		}
	}

	if !strings.Contains(path, "$") {
		return path, nil
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

		fallbackValid := make(chan bool, 1)
		go func() {
			infoFallback, errFallback := os.Stat(cfg.StartingDir)
			fallbackValid <- (errFallback == nil && infoFallback.IsDir())
		}()

		select {
		case valid := <-fallbackValid:
			if !valid {
				return "", fmt.Errorf("fallback STARTING_DIR %q is also invalid", cfg.StartingDir)
			}
		case <-time.After(100 * time.Millisecond):
			return "", fmt.Errorf("timeout checking fallback STARTING_DIR %q", cfg.StartingDir)
		}
	}

	var sb strings.Builder
	sb.Grow(128)
	sb.WriteString("cd ")
	sb.WriteString(shellQuote(cfg.StartingDir))
	sb.WriteString(" && ")
	sb.WriteString(defaultConfig.FzfCommand)
	sb.WriteString(" > ")
	sb.WriteString(shellQuote(tmpFzfOutput))
	fzfCommand := sb.String()

	var cmd *exec.Cmd

	if cfg.SpawnTerm {
		args := make([]string, 0, 8)

		if cfg.UseShellIC {
			if defaultConfig.ShellToUse == "" {
				shellDetectOnce.Do(detectUserShell)
			}

			shellInteractiveFlags := getShellInteractiveFlag(defaultConfig.ShellToUse)

			args = append(args, defaultConfig.WinTitleFlag, defaultConfig.WinTitle, "-e", defaultConfig.ShellToUse)
			args = append(args, shellInteractiveFlags...)
			args = append(args, fzfCommand)
		} else {
			args = append(args, defaultConfig.WinTitleFlag, defaultConfig.WinTitle, "-e", "/bin/sh", "-c", fzfCommand)
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
	if _, statErr := os.Stat(tmpFzfOutput); statErr == nil {
		os.Remove(tmpFzfOutput)
	}

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
	if !filepath.IsAbs(absolutePath) {
		absolutePath, err = filepath.Abs(absolutePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %q: %v\n", absolutePath, err)
			return "", nil
		}
	}

	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist or is inaccessible: %q (%v)\n", absolutePath, err)
		return "", nil
	}

	return absolutePath, nil
}

// shellQuote обрамляет строку кавычками
func shellQuote(s string) string {
	if !strings.Contains(s, "\"") {
		return "\"" + s + "\""
	}

	var sb strings.Builder
	sb.Grow(len(s) + 4)
	sb.WriteByte('"')
	sb.WriteString(strings.Replace(s, "\"", "\\\"", -1))
	sb.WriteByte('"')
	return sb.String()
}

// FileTypeInfo содержит информацию о типе файла
type FileTypeInfo struct {
	Path     string
	FileName string
	Ext      string
	MIMEType string
}

// Карты расширений файлов по типам приложений
var (
	extToPDFViewer   = map[string]struct{}{"pdf": {}}
	extToDocxViewer  = map[string]struct{}{"docx": {}, "doc": {}}
	extToImageViewer = map[string]struct{}{
		"png": {}, "jpg": {}, "jpeg": {}, "gif": {}, "bmp": {},
		"webp": {}, "svg": {}, "ico": {}, "tif": {}, "tiff": {},
	}
	extToVideoPlayer = map[string]struct{}{
		"flv": {}, "avi": {}, "mov": {}, "mp4": {}, "mkv": {}, "webm": {},
		"wmv": {}, "mpeg": {}, "mpg": {}, "mp3": {}, "ogg": {}, "oga": {},
		"wav": {}, "flac": {}, "opus": {}, "aac": {}, "m4a": {},
	}
	extToSpreadsheet = map[string]struct{}{"csv": {}, "tsv": {}, "ods": {}, "xlsx": {}}
	extToWebBrowser  = map[string]struct{}{"htm": {}, "html": {}, "xhtml": {}}
	extToTextEditor  = map[string]struct{}{
		"txt": {}, "md": {}, "markdown": {}, "sh": {}, "bash": {}, "zsh": {},
		"fish": {}, "py": {}, "rb": {}, "js": {}, "jsx": {}, "ts": {}, "tsx": {},
		"c": {}, "cpp": {}, "h": {}, "hpp": {}, "java": {}, "go": {}, "rs": {},
		"php": {}, "pl": {}, "lua": {}, "sql": {}, "json": {}, "yaml": {}, "yml": {},
		"toml": {}, "xml": {}, "css": {}, "scss": {}, "less": {}, "conf": {},
		"cfg": {}, "log": {}, "ini": {}, "desktop": {}, "service": {}, "env": {},
		"gitignore": {}, "dockerfile": {}, "": {},
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
		success := make(chan bool, 2)
		go func() { success <- launchApp(appAssociations.TextEditor, filePath) }()

		select {
		case result := <-success:
			if result {
				return nil
			}
			if launchApp(appAssociations.FallbackOpener, filePath) {
				return nil
			}
		case <-time.After(200 * time.Millisecond):
			if launchApp(appAssociations.FallbackOpener, filePath) {
				return nil
			}
		}

		return fmt.Errorf("could not open directory %q with any available application", filePath)
	}

	extWithDot := filepath.Ext(fileInfo.FileName)
	if extWithDot != "" {
		fileInfo.Ext = strings.ToLower(extWithDot[1:])
	} else if strings.HasPrefix(fileInfo.FileName, ".") {
		fileInfo.Ext = ""
	}

	var appToLaunch string

	if _, ok := extToPDFViewer[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.PDFViewer
	} else if _, ok := extToDocxViewer[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.DocxViewer
	} else if _, ok := extToImageViewer[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.ImageViewer
	} else if _, ok := extToVideoPlayer[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.VideoPlayer
	} else if _, ok := extToSpreadsheet[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.SpreadsheetEditor
	} else if _, ok := extToWebBrowser[fileInfo.Ext]; ok {
		appToLaunch = appAssociations.WebBrowser
	} else if _, ok := extToTextEditor[fileInfo.Ext]; ok {
		if fileInfo.Ext == "" {
			fileInfo.MIMEType = getMimeType(filePath)

			if fileInfo.MIMEType == "" ||
				strings.HasPrefix(fileInfo.MIMEType, mimeTextPrefix) ||
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
	mimeCache     = make(map[string]string, 100)
	mimeCacheLock sync.RWMutex
)

// getMimeType определяет MIME тип файла
func getMimeType(filePath string) string {
	mimeCacheLock.RLock()
	cachedMime, ok := mimeCache[filePath]
	mimeCacheLock.RUnlock()

	if ok {
		return cachedMime
	}

	ext := filepath.Ext(filePath)
	if ext != "" {
		lowerExt := strings.ToLower(ext)
		switch lowerExt {
		case ".txt", ".md", ".log", ".conf", ".cfg":
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimeTextPrefix + "plain"
			mimeCacheLock.Unlock()
			return mimeTextPrefix + "plain"
		case ".json":
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimeApplicationJSON
			mimeCacheLock.Unlock()
			return mimeApplicationJSON
		case ".xml":
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimeApplicationXML
			mimeCacheLock.Unlock()
			return mimeApplicationXML
		case ".pdf":
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimePDF
			mimeCacheLock.Unlock()
			return mimePDF
		case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".svg":
			mimeType := mimeImagePrefix + lowerExt[1:]
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimeType
			mimeCacheLock.Unlock()
			return mimeType
		case ".mp4", ".avi", ".mkv", ".mov":
			mimeType := mimeVideoPrefix + lowerExt[1:]
			mimeCacheLock.Lock()
			mimeCache[filePath] = mimeType
			mimeCacheLock.Unlock()
			return mimeType
		}
	}

	xdgMimePath, err := cachedLookPath("xdg-mime")
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, xdgMimePath, "query", "filetype", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	end := len(output)
	for end > 0 && (output[end-1] == '\n' || output[end-1] == '\r' || output[end-1] == ' ' || output[end-1] == '\t') {
		end--
	}

	start := 0
	for start < end && (output[start] == ' ' || output[start] == '\t') {
		start++
	}

	mimeType := string(output[start:end])

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

	if name == "cd" || name == "echo" || name == "exit" {
		pathCacheLock.Lock()
		pathCache[name] = name
		pathCacheLock.Unlock()
		return name, nil
	}

	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			pathCacheLock.Lock()
			pathCache[name] = name
			pathCacheLock.Unlock()
			return name, nil
		}
		return "", fmt.Errorf("executable %s not found", name)
	}

	resultChan := make(chan string, 1)
	errChan := make(chan error, 1)

	go func() {
		path, err := exec.LookPath(name)
		if err != nil {
			errChan <- err
			return
		}
		resultChan <- path
	}()

	select {
	case path := <-resultChan:
		pathCacheLock.Lock()
		pathCache[name] = path
		pathCacheLock.Unlock()
		return path, nil
	case err := <-errChan:
		return "", err
	case <-time.After(100 * time.Millisecond):
		return "", fmt.Errorf("timeout looking up path for %s", name)
	}
}

// launchApp запускает приложение для открытия файла
func launchApp(appCommand string, filePath string) bool {
	if appCommand == "" {
		return false
	}

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

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting application %q for file %q: %v\n", appCommand, filePath, err)
		return false
	}

	return true
}
