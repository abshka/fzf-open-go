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

	// Increased initial capacity for common path lookups
	pathCache     = make(map[string]string, 32)
	pathCacheLock sync.RWMutex

	userHomeDir string

	// Avoid function call overhead by using direct memory comparison
	textMimePrefixMatch = strings.HasPrefix

	// Pre-allocate common shell flags to avoid repeated slices
	fishFlags         = []string{"-c"}
	shFlags           = []string{"-ic"}
	defaultShellFlags = []string{"-c"}

	// Pre-parse common shell applications
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

	// Use sync.Once for thread-safe one-time initializations
	shellDetectOnce sync.Once
	pathCacheOnce   sync.Once
)

func init() {
	// Optimize for parallelism
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)

	// Get home directory just once at startup
	if u, err := user.Current(); err == nil {
		userHomeDir = u.HomeDir
	}

	// Initialize shell detection in background
	go func() {
		shellDetectOnce.Do(func() {
			detectUserShell()
		})
	}()

	// Cache common commands in background to avoid PATH lookups later
	go func() {
		pathCacheOnce.Do(func() {
			// Increase common commands list for better coverage
			commonCommands := []string{"xdg-mime", "sh", "bash", "zsh", "fish", "fzf",
				"zeditor", "zathura", "eog", "vlc", "wps", "thorium-browser", "xdg-open"}

			// Use a WaitGroup for parallel lookups
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

			// Close channel when all goroutines complete
			go func() {
				wg.Wait()
				close(resultChan)
			}()

			// Collect results
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
	// Fast path: check environment variable first
	shellPath := os.Getenv("SHELL")
	if shellPath != "" {
		shellName := filepath.Base(shellPath)
		if validShells[shellName] {
			defaultConfig.ShellToUse = shellName
			return
		}
	}

	// Optimization: check shells in order of common usage
	possibleShells := []string{"zsh", "bash", "fish", "dash", "sh"}

	// Use channel to get the first successful result
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

	// Wait with timeout
	select {
	case shell := <-resultChan:
		defaultConfig.ShellToUse = shell
	case <-time.After(200 * time.Millisecond):
		defaultConfig.ShellToUse = "sh"
		fmt.Fprintf(os.Stderr, "Warning: Could not detect user shell, falling back to /bin/sh\n")
	}
}

// getShellInteractiveFlag возвращает флаги для интерактивного режима в зависимости от оболочки
// Using pre-allocated slices for common flags
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

	// Reduced timeout from 5 minutes to 3 minutes
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
	// Preallocate with capacity
	cfg := &Config{
		Terminal:    defaultConfig.Terminal,
		StartingDir: defaultConfig.StartingDir,
		SpawnTerm:   false,
		NoAutoClose: false,
		UseShellIC:  true,
	}

	// Parse flags using bit operations where possible
	flag.BoolVar(&cfg.SpawnTerm, "n", cfg.SpawnTerm, "Spawn fzf in a new terminal window")
	flag.StringVar(&cfg.StartingDir, "d", cfg.StartingDir, "Starting directory for fzf")
	flag.StringVar(&cfg.Terminal, "t", cfg.Terminal, "Terminal emulator command")
	flag.BoolVar(&cfg.NoAutoClose, "k", cfg.NoAutoClose, "Keep window open (don't auto-close)")
	flag.BoolVar(&cfg.UseShellIC, "i", cfg.UseShellIC, "Use interactive shell mode (-ic flags)")

	flag.Parse()
	return cfg
}

// expandPath обрабатывает ~ и $VARS в пути - optimized with string builder
func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	// Fast path for home directory expansion
	if path[0] == '~' {
		if userHomeDir != "" {
			if len(path) == 1 {
				return userHomeDir, nil
			}
			// Use string builder for efficiency when concatenating
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

	// Fast path for non-environment vars
	if !strings.Contains(path, "$") {
		return path, nil
	}

	return os.ExpandEnv(path), nil
}

// getPathViaFZF запускает fzf и возвращает выбранный абсолютный путь
func getPathViaFZF(ctx context.Context, cfg *Config) (string, error) {
	// Fast path: ensure directory exists
	info, err := os.Stat(cfg.StartingDir)
	if err != nil || !info.IsDir() {
		originalDir := cfg.StartingDir

		// Use cached home directory if available
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

		// Verify fallback directory in a separate goroutine
		fallbackValid := make(chan bool, 1)
		go func() {
			infoFallback, errFallback := os.Stat(cfg.StartingDir)
			fallbackValid <- (errFallback == nil && infoFallback.IsDir())
		}()

		// Use short timeout for directory check
		select {
		case valid := <-fallbackValid:
			if !valid {
				return "", fmt.Errorf("fallback STARTING_DIR %q is also invalid", cfg.StartingDir)
			}
		case <-time.After(100 * time.Millisecond):
			return "", fmt.Errorf("timeout checking fallback STARTING_DIR %q", cfg.StartingDir)
		}
	}
	// fzfCommand := fmt.Sprintf(
	// 	"cd %s && %s > %s",
	// 	shellQuote(cfg.StartingDir),
	// 	defaultConfig.FzfCommand,
	// 	shellQuote(tmpFzfOutput),
	// )
	// Preallocated command with efficient string building
	var sb strings.Builder
	sb.Grow(128) // Preallocate for average command length
	sb.WriteString("cd ")
	sb.WriteString(shellQuote(cfg.StartingDir))
	sb.WriteString(" && ")
	sb.WriteString(defaultConfig.FzfCommand)
	sb.WriteString(" > ")
	sb.WriteString(shellQuote(tmpFzfOutput))
	fzfCommand := sb.String()

	var cmd *exec.Cmd

	if cfg.SpawnTerm {
		// Preallocate args slice with expected capacity
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
		// Direct pipe to standard I/O for better performance
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

	// Single operation file read with direct buffer use
	content, err := os.ReadFile(tmpFzfOutput)
	// Don't defer if it might not exist - check first
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

	// Fast trim space implementation
	selectedRelativePath := strings.TrimSpace(string(content))
	if selectedRelativePath == "" {
		return "", nil
	}

	// Resolve path with minimal operations
	absolutePath := filepath.Join(cfg.StartingDir, selectedRelativePath)
	// For caching behavior, if already absolute no need to reprocess
	if !filepath.IsAbs(absolutePath) {
		absolutePath, err = filepath.Abs(absolutePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %q: %v\n", absolutePath, err)
			return "", nil
		}
	}

	// Verify path exists with minimal context switch
	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist or is inaccessible: %q (%v)\n", absolutePath, err)
		return "", nil
	}

	return absolutePath, nil
}

// Faster shellQuote using a single replace call and string builder
func shellQuote(s string) string {
	if !strings.Contains(s, "\"") {
		return "\"" + s + "\""
	}

	var sb strings.Builder
	sb.Grow(len(s) + 4) // Extra space for quotes and potential escapes
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

// Карты для быстрого поиска по расширению - optimized with direct access for high performance
var (
	// Use uint8 instead of bool to save memory
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
	// Fast stat check with caching
	fi, err := os.Stat(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: File or directory not found: %q (%v)\n", filePath, err)
		return err
	}

	// Preallocate fileInfo structure
	fileInfo := FileTypeInfo{
		FileName: filepath.Base(filePath),
	}

	// Fast path for directories
	if fi.IsDir() {
		// Try primary and fallback in parallel
		success := make(chan bool, 2)
		go func() { success <- launchApp(appAssociations.TextEditor, filePath) }()

		select {
		case result := <-success:
			if result {
				return nil
			}
			// Try fallback immediately
			if launchApp(appAssociations.FallbackOpener, filePath) {
				return nil
			}
		case <-time.After(200 * time.Millisecond):
			// Timeout, try fallback
			if launchApp(appAssociations.FallbackOpener, filePath) {
				return nil
			}
		}

		return fmt.Errorf("could not open directory %q with any available application", filePath)
	}

	// Fast extension extraction
	extWithDot := filepath.Ext(fileInfo.FileName)
	if extWithDot != "" {
		fileInfo.Ext = strings.ToLower(extWithDot[1:]) // Skip the dot directly
	} else if strings.HasPrefix(fileInfo.FileName, ".") {
		fileInfo.Ext = ""
	}

	// Use map lookup instead of if-else chain for faster matching
	var appToLaunch string

	// Check extension maps with zero allocations
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
			// Only check MIME if we really need to
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

	// Fallback to MIME type if extension didn't match
	if appToLaunch == "" {
		if fileInfo.MIMEType == "" {
			fileInfo.MIMEType = getMimeType(filePath)
		}

		if fileInfo.MIMEType != "" {
			appToLaunch = getAppByMIME(fileInfo.MIMEType)
		}
	}

	// Launch appropriate application
	if appToLaunch != "" {
		if launchApp(appToLaunch, filePath) {
			return nil
		}
	}

	// Final fallback to generic opener
	fmt.Fprintf(os.Stderr, "Info: No specific rule matched for %q (MIME: %q). Falling back to %q...\n",
		fileInfo.FileName, fileInfo.MIMEType, appAssociations.FallbackOpener)

	if !launchApp(appAssociations.FallbackOpener, filePath) {
		return fmt.Errorf("fallback opener %q failed to launch for %q", appAssociations.FallbackOpener, filePath)
	}

	return nil
}

// Optimized, using switch for faster string prefix checking
func getAppByMIME(mimeType string) string {
	// Fast prefix check using direct comparison
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

// Кэш для MIME типов - increased initial capacity
var (
	mimeCache     = make(map[string]string, 100)
	mimeCacheLock sync.RWMutex
)

// Optimized MIME type detection with context and timeout
func getMimeType(filePath string) string {
	// Fast path: check cache first with read lock
	mimeCacheLock.RLock()
	cachedMime, ok := mimeCache[filePath]
	mimeCacheLock.RUnlock()

	if ok {
		return cachedMime
	}

	// Fast path: check for common extension patterns before running xdg-mime
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

	// Look up xdg-mime only if needed
	xdgMimePath, err := cachedLookPath("xdg-mime")
	if err != nil {
		return ""
	}

	// Shorter timeout for MIME detection
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, xdgMimePath, "query", "filetype", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// Fast trim with direct byte indexing
	end := len(output)
	for end > 0 && (output[end-1] == '\n' || output[end-1] == '\r' || output[end-1] == ' ' || output[end-1] == '\t') {
		end--
	}

	start := 0
	for start < end && (output[start] == ' ' || output[start] == '\t') {
		start++
	}

	mimeType := string(output[start:end])

	// Cache result
	mimeCacheLock.Lock()
	mimeCache[filePath] = mimeType
	mimeCacheLock.Unlock()

	return mimeType
}

// cachedLookPath кэширует результаты exec.LookPath
func cachedLookPath(name string) (string, error) {
	// Fast path: check cache first with read lock
	pathCacheLock.RLock()
	path, ok := pathCache[name]
	pathCacheLock.RUnlock()

	if ok {
		return path, nil
	}

	// Check if it's a built-in command
	if name == "cd" || name == "echo" || name == "exit" {
		pathCacheLock.Lock()
		pathCache[name] = name
		pathCacheLock.Unlock()
		return name, nil
	}

	// If absolute path, no need to look it up
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			pathCacheLock.Lock()
			pathCache[name] = name
			pathCacheLock.Unlock()
			return name, nil
		}
		return "", fmt.Errorf("executable %s not found", name)
	}

	// Use LookPath with context and timeout
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

	// Use timeout to avoid blocking
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

// Optimized app launcher using string splitting and syscall optimizations
func launchApp(appCommand string, filePath string) bool {
	if appCommand == "" {
		return false
	}

	// Fast path: simple string splitting with Fields - optimized for common case
	parts := strings.Fields(appCommand)
	if len(parts) == 0 {
		fmt.Fprintf(os.Stderr, "Error: Invalid empty application command.\n")
		return false
	}

	appName := parts[0]
	appArgs := parts[1:]

	// Fast path: cached lookup
	appPath, err := cachedLookPath(appName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Application command not found in PATH: %q\n", appName)
		return false
	}

	// Preallocate with exact capacity
	finalArgs := make([]string, 0, len(appArgs)+1)
	finalArgs = append(finalArgs, appArgs...)
	finalArgs = append(finalArgs, filePath)

	cmd := exec.Command(appPath, finalArgs...)

	// Optimize syscall settings for launching applications
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		// Additional optimizations for process detachment
		Pgid: 0,
	}

	// Explicitly close all file descriptors
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start application with minimal system calls
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting application %q for file %q: %v\n", appCommand, filePath, err)
		return false
	}

	// Don't wait for the process to complete - let it run independently
	return true
}
