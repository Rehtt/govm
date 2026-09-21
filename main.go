package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Rehtt/Kit/cli"
)

var version = "dev"

func main() {
	root := cli.NewCLI("govm", "跨平台 Go 版本管理器")
	root.Usage = "[command]"
	_ = root.AddCommand(
		install(),
		list(),
		listRemote(),
		use(),
		uninstall(),
		cacheCommand(),
		initCommand(),
		completionCommand(),
		versionCommand(),
	)

	if err := root.Run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func versionCommand() *cli.CLI {
	c := cli.NewCLI("version", "查看 govm 版本")
	c.Usage = "[flags]"
	c.CommandFunc = func(args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("version does not accept positional arguments")
		}
		fmt.Printf("govm %s\n", version)
		return nil
	}
	return c
}

func install() *cli.CLI {
	c := cli.NewCLI("install", "安装指定版本的 Go")
	c.Usage = "[flags] VERSION..."
	jobs := c.IntShortLong("j", "jobs", 2, "并发安装数（源码构建默认为 1）")
	build := c.Bool("build", false, "从官方源码构建，而不是安装二进制归档")
	force := c.Bool("force", false, "覆盖已安装的版本")
	noInit := c.Bool("no-init", false, "跳过 shell 配置")
	c.CommandFunc = func(args []string) error {
		var trailingJobsSet bool
		var err error
		args, trailingJobsSet, err = parseTrailingInstallArgs(args, jobs, build, force, noInit)
		if err != nil {
			return err
		}
		if len(args) == 0 {
			return fmt.Errorf("version is required")
		}
		if *jobs < 1 && (flagWasSet(c, "j", "jobs") || trailingJobsSet) {
			return fmt.Errorf("jobs must be at least 1")
		}
		workerCount := *jobs
		if *build && !flagWasSet(c, "j", "jobs") && !trailingJobsSet {
			workerCount = 1
		}
		return installVersions(args, InstallOptions{
			Build:       *build,
			Force:       *force,
			NoInit:      *noInit,
			Jobs:        workerCount,
			Environment: DefaultEnvironment(),
		})
	}
	return c
}

func list() *cli.CLI {
	c := cli.NewCLI("list", "列出已安装的 Go 版本")
	c.Usage = "[flags]"
	jsonOutput := c.Bool("json", false, "以 JSON 输出")
	c.CommandFunc = func(args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("list does not accept positional arguments")
		}
		return listGoWithOptions(DefaultEnvironment(), *jsonOutput)
	}
	return c
}

func listRemote() *cli.CLI {
	c := cli.NewCLI("list-remote", "列出可以安装的 Go 版本")
	c.Usage = "[flags]"
	all := c.Bool("all", false, "包含预发布和归档版本")
	refresh := c.Bool("refresh", false, "忽略本地元数据缓存")
	jsonOutput := c.Bool("json", false, "以 JSON 输出")
	c.CommandFunc = func(args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("list-remote does not accept positional arguments")
		}
		return listRemoteGoWithOptions(DefaultEnvironment(), RemoteListOptions{
			All:     *all,
			Refresh: *refresh,
			JSON:    *jsonOutput,
		})
	}
	return c
}

func use() *cli.CLI {
	c := cli.NewCLI("use", "切换当前 Go 版本")
	c.Usage = "[flags] VERSION"
	setDefault := c.BoolShortLong("d", "default", false, "同时设置为默认版本")
	c.CommandFunc = func(args []string) error {
		var err error
		args, err = parseTrailingBoolArgs(args, map[string]*bool{"-d": setDefault, "--default": setDefault})
		if err != nil {
			return err
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: govm use VERSION")
		}
		return useGoWithEnvironment(DefaultEnvironment(), args[0], *setDefault)
	}
	return c
}

func uninstall() *cli.CLI {
	c := cli.NewCLI("uninstall", "卸载已安装的 Go 版本")
	c.Usage = "[flags] VERSION"
	force := c.Bool("force", false, "允许删除 current 或 default")
	c.CommandFunc = func(args []string) error {
		var err error
		args, err = parseTrailingBoolArgs(args, map[string]*bool{"--force": force})
		if err != nil {
			return err
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: govm uninstall VERSION")
		}
		return uninstallGo(DefaultEnvironment(), args[0], *force)
	}
	return c
}

func cacheCommand() *cli.CLI {
	c := cli.NewCLI("cache", "管理下载和元数据缓存")
	clean := cli.NewCLI("clean", "清理缓存")
	clean.Usage = "[flags]"
	all := clean.Bool("all", false, "同时清理自动下载的 bootstrap 工具链")
	clean.CommandFunc = func(args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("cache clean does not accept positional arguments")
		}
		return cleanCache(DefaultEnvironment(), *all)
	}
	_ = c.AddCommand(clean)
	return c
}

func initCommand() *cli.CLI {
	c := cli.NewCLI("init", "写入或修复 shell 配置")
	c.Usage = "[SHELL]"
	c.CommandFunc = func(args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("usage: govm init [bash|zsh|fish|powershell]")
		}
		shell := ""
		if len(args) == 1 {
			shell = args[0]
		}
		return initShell(DefaultEnvironment(), shell)
	}
	return c
}

func completionCommand() *cli.CLI {
	c := cli.NewCLI("completion", "生成 shell 补全脚本")
	c.Usage = "SHELL"
	c.CommandFunc = func(args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("usage: govm completion [bash|zsh|fish|powershell]")
		}
		return printCompletion(args[0])
	}
	return c
}

func flagWasSet(c *cli.CLI, names ...string) bool {
	set := false
	c.Visit(func(f *flag.Flag) {
		for _, name := range names {
			if f.Name == name {
				set = true
				return
			}
		}
	})
	return set
}

// The standard flag package stops parsing at the first positional argument.
// govm accepts the documented VERSION... [flags] spelling as well as the
// conventional [flags] VERSION... form, so the small trailing parsers below
// normalize only flags known by each command.
func parseTrailingInstallArgs(args []string, jobs *int, build, force, noInit *bool) ([]string, bool, error) {
	positional := make([]string, 0, len(args))
	jobsSet := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			return positional, jobsSet, nil
		case arg == "--build":
			*build = true
		case strings.HasPrefix(arg, "--build="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--build="))
			if err != nil {
				return nil, false, fmt.Errorf("invalid value for --build")
			}
			*build = value
		case arg == "--force":
			*force = true
		case strings.HasPrefix(arg, "--force="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--force="))
			if err != nil {
				return nil, false, fmt.Errorf("invalid value for --force")
			}
			*force = value
		case arg == "--no-init":
			*noInit = true
		case strings.HasPrefix(arg, "--no-init="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--no-init="))
			if err != nil {
				return nil, false, fmt.Errorf("invalid value for --no-init")
			}
			*noInit = value
		case arg == "-j" || arg == "--jobs":
			if i+1 >= len(args) {
				return nil, false, fmt.Errorf("%s requires a value", arg)
			}
			i++
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 1 {
				return nil, false, fmt.Errorf("invalid jobs value %q", args[i])
			}
			*jobs = value
			jobsSet = true
		case strings.HasPrefix(arg, "--jobs="):
			value, err := strconv.Atoi(strings.TrimPrefix(arg, "--jobs="))
			if err != nil || value < 1 {
				return nil, false, fmt.Errorf("invalid jobs value %q", arg)
			}
			*jobs = value
			jobsSet = true
		case strings.HasPrefix(arg, "-j") && len(arg) > 2:
			valueText := strings.TrimPrefix(arg, "-j")
			valueText = strings.TrimPrefix(valueText, "=")
			value, err := strconv.Atoi(valueText)
			if err != nil || value < 1 {
				return nil, false, fmt.Errorf("invalid jobs value %q", arg)
			}
			*jobs = value
			jobsSet = true
		case strings.HasPrefix(arg, "-"):
			return nil, false, fmt.Errorf("unknown install flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	return positional, jobsSet, nil
}

func parseTrailingBoolArgs(args []string, flags map[string]*bool) ([]string, error) {
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positional, args[i+1:]...), nil
		}
		if target, ok := flags[arg]; ok {
			*target = true
			continue
		}
		matched := false
		for name, target := range flags {
			prefix := name + "="
			if strings.HasPrefix(arg, prefix) {
				value, err := strconv.ParseBool(strings.TrimPrefix(arg, prefix))
				if err != nil {
					return nil, fmt.Errorf("invalid value for %s", name)
				}
				*target = value
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		positional = append(positional, arg)
	}
	return positional, nil
}
