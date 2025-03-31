package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	// "syscall" // Убираем импорт syscall, он больше не нужен
)

const (
	resourcesPath    = "/usr/share/fzf-open/"
	configRelPath    = ".config/fzf-open/"
	lopenScriptName  = "lopen.sh"
	configScriptName = "config"
	tmpFzfOutput     = "/tmp/fzf-open"
)

// Config структура для хранения конфигурации
type Config struct {
	Opener          string
	Terminal        string
	StartingDir     string
	WinTitleFlag    string
	WinTitle        string
	SpawnTerm       bool
	ConfigPath      string
	LopenScriptPath string
	ConfigFilepath  string
}

func main() {
	cfg, err := initializeConfig()
	if err != nil {
		log.Fatalf("Error initializing configuration: %v", err)
	}

	err = ensureConfigDirAndFiles(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Error ensuring config directory/files: %v\n", err)
	}

	err = readConfigFile(cfg)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "Warning: Error reading config file '%s': %v\n", cfg.ConfigFilepath, err)
	}

	readFlags(cfg)

	err = expandConfigPaths(cfg)
	if err != nil {
		log.Fatalf("Error expanding configuration paths: %v", err)
	}

	err = checkOpenerExecutable(cfg.Opener)
	if err != nil {
		log.Fatalf("Opener script check failed: %v", err)
	}

	selectedPath, err := getPathViaFZF(cfg)
	if err != nil {
		// Ошибки fzf (включая отмену) уже обработаны внутри getPathViaFZF
		os.Exit(0) // Просто выходим, если путь не выбран или ошибка обработана
	}

	if selectedPath == "" {
		os.Exit(0)
	}

	err = openFile(selectedPath, cfg)
	if err != nil {
		// Ошибка уже выведена в openFile
		os.Exit(1) // Выходим с кодом ошибки
	}
}

// initializeConfig устанавливает дефолтные значения и пути
func initializeConfig() (*Config, error) {
	currentUser, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("could not get current user: %w", err)
	}
	homeDir := currentUser.HomeDir

	configPath := filepath.Join(homeDir, configRelPath)
	lopenPath := filepath.Join(configPath, lopenScriptName)
	configFilePath := filepath.Join(configPath, configScriptName)

	cfg := &Config{
		Opener:          lopenPath,
		Terminal:        "alacritty",
		StartingDir:     homeDir,
		WinTitleFlag:    "--title",
		WinTitle:        "fzf-open-run",
		SpawnTerm:       false,
		ConfigPath:      configPath,
		LopenScriptPath: lopenPath,
		ConfigFilepath:  configFilePath,
	}
	return cfg, nil
}

// ensureConfigDirAndFiles создает директорию и копирует/создает файлы конфига
func ensureConfigDirAndFiles(cfg *Config) error {
	err := os.MkdirAll(cfg.ConfigPath, 0755)
	if err != nil {
		return fmt.Errorf("could not create config directory '%s': %w", cfg.ConfigPath, err)
	}

	if _, err := os.Stat(cfg.LopenScriptPath); errors.Is(err, os.ErrNotExist) {
		defaultLopenPath := filepath.Join(resourcesPath, lopenScriptName)
		if _, err := os.Stat(defaultLopenPath); err == nil {
			err = copyFile(defaultLopenPath, cfg.LopenScriptPath)
			if err != nil {
				return fmt.Errorf("could not copy default lopen.sh: %w", err)
			}
			err = os.Chmod(cfg.LopenScriptPath, 0775) // rwxrwxr-x
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not set lopen.sh executable: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Warning: Default lopen.sh not found at '%s'. Opener might not work.\n", defaultLopenPath)
		}
	} else if err != nil {
		return fmt.Errorf("could not check status of '%s': %w", cfg.LopenScriptPath, err)
	}

	if _, err := os.Stat(cfg.ConfigFilepath); errors.Is(err, os.ErrNotExist) {
		file, err := os.Create(cfg.ConfigFilepath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not create empty config file '%s': %v\n", cfg.ConfigFilepath, err)
		} else {
			file.Close()
		}
	} else if err != nil {
		return fmt.Errorf("could not check status of '%s': %w", cfg.ConfigFilepath, err)
	}

	return nil
}

// readConfigFile читает ~/.config/fzf-open/config
func readConfigFile(cfg *Config) error {
	file, err := os.Open(cfg.ConfigFilepath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		// Используем strings.TrimSpace с большой буквы
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Warning: Skipping invalid line %d in config: '%s'\n", lineNumber, line)
			continue
		}

		// Используем strings.TrimSpace с большой буквы
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "OPENER":
			cfg.Opener = value
		case "TERMINAL":
			cfg.Terminal = value
		case "STARTING_DIR":
			cfg.StartingDir = value
		case "WIN_TITLE_FLAG":
			cfg.WinTitleFlag = value
		case "WIN_TITLE":
			cfg.WinTitle = value
		case "SPAWN_TERM":
			// Используем strings.ToLower с большой буквы
			boolVal, err := strconv.ParseBool(strings.ToLower(value))
			if err == nil {
				cfg.SpawnTerm = boolVal
			} else {
				fmt.Fprintf(os.Stderr, "Warning: Invalid boolean value for SPAWN_TERM on line %d: '%s'\n", lineNumber, value)
			}
		default:
			// fmt.Fprintf(os.Stderr, "Warning: Unknown key '%s' in config file.\n", key)
		}
	}

	return scanner.Err()
}

// readFlags читает флаги командной строки
func readFlags(cfg *Config) {
	flag.BoolVar(&cfg.SpawnTerm, "n", cfg.SpawnTerm, "Spawn fzf in a new terminal window")
	flag.StringVar(&cfg.Opener, "o", cfg.Opener, "Path to the opener script")
	flag.StringVar(&cfg.StartingDir, "d", cfg.StartingDir, "Starting directory for fzf")
	flag.StringVar(&cfg.Terminal, "t", cfg.Terminal, "Terminal emulator command")
	flag.Parse()
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

// expandConfigPaths применяет expandPath к нужным полям конфига
func expandConfigPaths(cfg *Config) error {
	var err error
	cfg.Opener, err = expandPath(cfg.Opener)
	if err != nil {
		return fmt.Errorf("expanding OPENER path '%s': %w", cfg.Opener, err)
	}
	cfg.StartingDir, err = expandPath(cfg.StartingDir)
	if err != nil {
		return fmt.Errorf("expanding STARTING_DIR path '%s': %w", cfg.StartingDir, err)
	}
	cfg.LopenScriptPath, err = expandPath(cfg.LopenScriptPath)
	if err != nil {
		return fmt.Errorf("expanding LopenScriptPath path '%s': %w", cfg.LopenScriptPath, err)
	}
	cfg.ConfigFilepath, err = expandPath(cfg.ConfigFilepath)
	if err != nil {
		return fmt.Errorf("expanding ConfigFilepath path '%s': %w", cfg.ConfigFilepath, err)
	}
	cfg.ConfigPath, err = expandPath(cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("expanding ConfigPath path '%s': %w", cfg.ConfigPath, err)
	}
	return nil
}

// checkOpenerExecutable проверяет, что скрипт существует и исполняемый
func checkOpenerExecutable(openerPath string) error {
	info, err := os.Stat(openerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("configured OPENER script not found: '%s'", openerPath)
		}
		return fmt.Errorf("could not stat OPENER script '%s': %w", openerPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("configured OPENER path is a directory, not a file: '%s'", openerPath)
	}

	// Проверка прав на исполнение через биты режима файла (более переносимо)
	// Проверяем бит execute для владельца, группы или остальных
	if info.Mode()&0111 == 0 {
		// Убрали проверку через syscall.Access
		return fmt.Errorf("configured OPENER script is not executable: '%s'. Please run 'chmod +x \"%s\"'", openerPath, openerPath)
	}
	return nil
}

// getPathViaFZF запускает fzf и возвращает выбранный абсолютный путь
func getPathViaFZF(cfg *Config) (string, error) {
	// --- Проверка StartingDir (без изменений) ---
	if info, err := os.Stat(cfg.StartingDir); err != nil || !info.IsDir() {
		originalDir := cfg.StartingDir
		currentUser, _ := user.Current()
		cfg.StartingDir = currentUser.HomeDir // Fallback to home
		fmt.Fprintf(os.Stderr, "Warning: STARTING_DIR '%s' is invalid, falling back to '%s'\n", originalDir, cfg.StartingDir)
		if infoFallback, errFallback := os.Stat(cfg.StartingDir); errFallback != nil || !infoFallback.IsDir() {
			return "", fmt.Errorf("fallback STARTING_DIR '%s' is also invalid", cfg.StartingDir)
		}
	}

	// --- Формирование команды fzf (без изменений) ---
	fzfCommand := fmt.Sprintf(
		`cd "%s"; fzf --prompt='Select file> ' --border --no-multi > "%s"`,
		cfg.StartingDir,
		tmpFzfOutput,
	)

	var cmd *exec.Cmd
	var err error

	if cfg.SpawnTerm {
		// --- Запуск в новом терминале (без изменений) ---
		args := []string{
			cfg.WinTitleFlag,
			cfg.WinTitle,
			"-e",
			"/bin/sh", "-c",
			fzfCommand,
		}
		cmd = exec.Command(cfg.Terminal, args...)
		err = cmd.Run() // Запускаем и ждем завершения терминала

	} else {
		// --- Запуск в ТЕКУЩЕЙ среде ---
		cmd = exec.Command("/bin/zsh", "-c", fzfCommand)

		// !!! ВАЖНО: Подключаем stdin, stdout, stderr !!!
		// Это позволяет fzf взаимодействовать с текущим терминалом
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout // fzf UI обычно рисуется в stdout или stderr
		cmd.Stderr = os.Stderr // Подключим и stderr на всякий случай

		// Запускаем команду и ждем ее завершения
		err = cmd.Run()

		// !!! ВАЖНО: После cmd.Run() stdout/stderr снова принадлежат Go программе !!!
		// Вывод fzf (выбранный файл) все равно будет перенаправлен в /tmp/fzf-open
		// командой `> "%s"` внутри fzfCommand, так что подключение cmd.Stdout к os.Stdout
		// не помешает получить результат из файла. Оно нужно только для интерактивной работы fzf.
	}

	// --- Обработка ошибок запуска cmd (без изменений) ---
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 130 { // Отмена пользователем
				return "", nil
			}
			// Не выводим ошибку здесь, если это просто ненулевой код выхода fzf (например, 1 при ошибке поиска)
			// fmt.Fprintf(os.Stderr, "Error running fzf command (code %d): %v\n", exitErr.ExitCode(), err)
			// Возвращаем nil, т.к. файл результата может быть не создан или пуст
			return "", nil
		}
		// Ошибка запуска самой команды (sh, терминал не найден и т.д.)
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		return "", err // Возвращаем реальную ошибку запуска
	}

	// --- Чтение результата из файла (без изменений) ---
	defer os.Remove(tmpFzfOutput)
	content, err := os.ReadFile(tmpFzfOutput)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) { // Файл не создан (fzf мог завершиться без выбора)
			return "", nil
		}
		fmt.Fprintf(os.Stderr, "Error reading fzf output file '%s': %v\n", tmpFzfOutput, err)
		// Возвращаем nil, т.к. это не критическая ошибка программы, а проблема fzf/файла
		return "", nil
	}

	selectedRelativePath := strings.TrimSpace(string(content))
	if selectedRelativePath == "" {
		return "", nil
	}

	absolutePath := filepath.Join(cfg.StartingDir, selectedRelativePath)

	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Constructed path does not exist: '%s'\n", absolutePath)
		return "", nil
	}

	return absolutePath, nil
}

// openFile запускает скрипт-опенер с выбранным путем
func openFile(selectedPath string, cfg *Config) error {
	cmd := exec.Command(cfg.Opener, selectedPath)
	output, err := cmd.CombinedOutput()

	if err != nil {
		// Выводим ошибку и вывод скрипта-опенера
		fmt.Fprintf(os.Stderr, "Error running opener script '%s': %v\n", cfg.Opener, err)
		if len(output) > 0 {
			// Используем strings.TrimSpace с большой буквы
			fmt.Fprintf(os.Stderr, "Opener Output/Error:\n%s\n", strings.TrimSpace(string(output)))
		}
		return err // Возвращаем ошибку, чтобы main мог выйти с ненулевым кодом
	}
	return nil
}

// copyFile копирует файл
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("could not open source file '%s': %w", src, err)
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("could not create destination file '%s': %w", dst, err)
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		return fmt.Errorf("could not copy content from '%s' to '%s': %w", src, dst, err)
	}

	sourceInfo, err := os.Stat(src)
	if err == nil {
		os.Chmod(dst, sourceInfo.Mode())
	}

	return nil
}
