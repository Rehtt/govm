package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	shellBlockStart = "# >>> govm initialize >>>"
	shellBlockEnd   = "# <<< govm initialize <<<"
)

func initShell(environment *Environment, requestedShell string) error {
	env, err := environment.normalized()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(env.Root, 0o755); err != nil {
		return err
	}
	shell := normalizeShell(requestedShell)
	if shell == "" {
		shell = detectShell()
	}
	if !supportedShell(shell) {
		fmt.Fprintf(env.Output, "govm: shell %q is not supported; use bash, zsh, fish, or powershell\n", shell)
		return nil
	}
	configPath := shellConfigPath(env, shell)
	old, mode, existed, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("read %s configuration: %w", configPath, err)
	}
	updated := updateManagedBlock(old, shellBlock(env, shell))
	if updated == old {
		fmt.Fprintf(env.Output, "govm: %s configuration already initialized (%s)\n", shell, configPath)
		return nil
	}
	if existed {
		backupPath := configPath + ".govm.bak"
		if _, statErr := os.Stat(backupPath); errors.Is(statErr, os.ErrNotExist) {
			if err := writeFileAtomic(backupPath, []byte(old), mode); err != nil {
				return fmt.Errorf("backup %s configuration: %w", shell, err)
			}
		}
	}
	if err := writeFileAtomic(configPath, []byte(updated), mode); err != nil {
		return fmt.Errorf("write %s configuration: %w", shell, err)
	}
	fmt.Fprintf(env.Output, "initialized %s (%s)\n", shell, configPath)
	return nil
}

func normalizeShell(shell string) string {
	shell = strings.ToLower(strings.TrimSpace(shell))
	shell = strings.TrimSuffix(shell, ".exe")
	if index := strings.LastIndexAny(shell, `/\\`); index >= 0 {
		shell = shell[index+1:]
	}
	return shell
}

func detectShell() string {
	if shell := normalizeShell(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	if os.Getenv("PSModulePath") != "" || os.Getenv("ComSpec") != "" {
		return "powershell"
	}
	return ""
}

func supportedShell(shell string) bool {
	switch normalizeShell(shell) {
	case "bash", "zsh", "fish", "powershell", "pwsh":
		return true
	default:
		return false
	}
}

func shellConfigPath(env *Environment, shell string) string {
	home := filepath.Dir(env.Root)
	switch normalizeShell(shell) {
	case "bash":
		return filepath.Join(home, ".bashrc")
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "fish":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, "fish", "config.fish")
	case "powershell", "pwsh":
		if profile := os.Getenv("GOVM_POWERSHELL_PROFILE"); profile != "" {
			return profile
		}
		if os.Getenv("ComSpec") != "" {
			return filepath.Join(home, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1")
		}
		return filepath.Join(home, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1")
	default:
		return filepath.Join(home, ".profile")
	}
}

func readConfig(path string) (content string, mode os.FileMode, existed bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0o644, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, false, err
	}
	mode = info.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	return string(data), mode, true, nil
}

func updateManagedBlock(content, block string) string {
	for {
		start := strings.Index(content, shellBlockStart)
		if start < 0 {
			break
		}
		relEnd := strings.Index(content[start+len(shellBlockStart):], shellBlockEnd)
		if relEnd < 0 {
			content = content[:start]
			break
		}
		end := start + len(shellBlockStart) + relEnd + len(shellBlockEnd)
		content = content[:start] + content[end:]
	}
	content = strings.TrimRight(content, "\r\n")
	if content != "" {
		content += "\n\n"
	}
	return content + block + "\n"
}

func shellBlock(env *Environment, shell string) string {
	path := filepath.Join(env.Root, "current", "bin")
	shell = normalizeShell(shell)
	switch shell {
	case "fish":
		return shellBlockStart + "\nset -gx PATH " + shellQuote(path) + " $PATH\n" + shellBlockEnd
	case "powershell", "pwsh":
		return shellBlockStart + "\n$env:Path = " + powershellQuote(path+string(os.PathListSeparator)) + " + $env:Path\n" + shellBlockEnd
	default:
		return shellBlockStart + "\nexport PATH=" + shellQuote(path) + ":$PATH\n" + shellBlockEnd
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powershellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func printCompletion(shell string) error {
	switch normalizeShell(shell) {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "fish":
		fmt.Print(fishCompletion)
	case "powershell", "pwsh":
		fmt.Print(powerShellCompletion)
	default:
		return fmt.Errorf("unsupported shell %q; choose bash, zsh, fish, or powershell", shell)
	}
	return nil
}

const bashCompletion = `# bash completion for govm
_govm_complete() {
    local cur prev words cword
    _init_completion || return
    local commands="install list list-remote use uninstall cache init completion"
    if [[ ${cword} -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "${commands}" -- "${cur}") )
        return
    fi
    case "${words[1]}" in
        install) COMPREPLY=( $(compgen -W "--build --force --no-init -j --jobs" -- "${cur}") ) ;;
        list) COMPREPLY=( $(compgen -W "--json" -- "${cur}") ) ;;
        list-remote) COMPREPLY=( $(compgen -W "--all --refresh --json" -- "${cur}") ) ;;
        use) COMPREPLY=( $(compgen -W "-d --default" -- "${cur}") ) ;;
        uninstall) COMPREPLY=( $(compgen -W "--force" -- "${cur}") ) ;;
        cache) COMPREPLY=( $(compgen -W "clean" -- "${cur}") ) ;;
        init|completion) COMPREPLY=( $(compgen -W "bash zsh fish powershell" -- "${cur}") ) ;;
    esac
}
complete -F _govm_complete govm
`

const zshCompletion = `#compdef govm
_govm() {
  _arguments '1:command:(install list list-remote use uninstall cache init completion)' '*::arg:->args'
  case $words[2] in
    install) _arguments '--build[build from source]' '--force[replace]' '--no-init[skip shell init]' '-j+[parallel jobs]:jobs:' '--jobs+[parallel jobs]:jobs:' ;;
    list) _arguments '--json[JSON output]' ;;
    list-remote) _arguments '--all[include prereleases]' '--refresh[refresh metadata]' '--json[JSON output]' ;;
    use) _arguments '-d[set default]' '--default[set default]' ;;
    uninstall) _arguments '--force[remove current/default]' ;;
    cache) _arguments '1:subcommand:(clean)' ;;
    init|completion) _arguments '1:shell:(bash zsh fish powershell)' ;;
  esac
}
compdef _govm govm
`

const fishCompletion = `complete -c govm -f -n '__fish_use_subcommand' -a 'install list list-remote use uninstall cache init completion'
complete -c govm -f -n '__fish_seen_subcommand_from init completion' -a 'bash zsh fish powershell'
complete -c govm -l build -n '__fish_seen_subcommand_from install'
complete -c govm -l force -n '__fish_seen_subcommand_from install uninstall'
complete -c govm -l no-init -n '__fish_seen_subcommand_from install'
complete -c govm -s j -l jobs -r -n '__fish_seen_subcommand_from install'
complete -c govm -l json -n '__fish_seen_subcommand_from list list-remote'
complete -c govm -l all -n '__fish_seen_subcommand_from list-remote'
complete -c govm -l refresh -n '__fish_seen_subcommand_from list-remote'
complete -c govm -s d -l default -n '__fish_seen_subcommand_from use'
complete -c govm -f -n '__fish_seen_subcommand_from cache' -a clean
complete -c govm -l all -n '__fish_seen_subcommand_from clean'
`

const powerShellCompletion = `Register-ArgumentCompleter -Native -CommandName govm -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $commands = 'install','list','list-remote','use','uninstall','cache','init','completion'
    $tokens = $commandAst.CommandElements | ForEach-Object { $_.Extent.Text }
    if ($tokens.Count -le 1) {
        $commands | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
    }
}
`
