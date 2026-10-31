package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"
)

// aliasName keeps --alias to names that are safe to print into a shell script, and
// that no shell reads as an option.
var aliasName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

func newInit() *cobra.Command {
	var alias string
	cmd := &cobra.Command{
		Use:   "init <zsh|bash|fish|ssh> [--alias NAME]",
		Short: "Print the shell integration (completion, optional alias) or the SSH setup",
		Long: `Print the shell integration (completion, optional alias) or the SSH setup.

    zsh:   eval "$(hop init zsh)"           in ~/.zshrc, after compinit
    bash:  eval "$(hop init bash)"          in ~/.bashrc
    fish:  hop init fish | source           in ~/.config/fish/config.fish
    ssh:   hop init ssh >> ~/.ssh/config    then: ssh ec2-user@tools.hop

--alias NAME also makes NAME an alias for hop, with the same completion, and removes
any shell function of that name that would shadow it. For people used to typing
another name:

    eval "$(hop init zsh --alias ssm)"

For PowerShell, see "hop completion powershell --help".`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"zsh", "bash", "fish", "ssh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			switch target {
			case "zsh", "bash", "fish":
			case "ssh":
				if alias != "" {
					return errors.New("--alias is for shells: hop init ssh prints a ~/.ssh/config block")
				}
				fmt.Fprint(os.Stdout, sshConfig(sshCommand()))
				return nil
			default:
				return unsupportedShell(target)
			}
			if alias != "" && !aliasName.MatchString(alias) {
				return fmt.Errorf("invalid alias name %q: use letters, digits, _ and -, not first", alias)
			}
			var comp bytes.Buffer
			var err error
			switch target {
			case "zsh":
				err = cmd.Root().GenZshCompletion(&comp)
			case "bash":
				err = cmd.Root().GenBashCompletionV2(&comp, true)
			case "fish":
				err = cmd.Root().GenFishCompletion(&comp, true)
			}
			if err != nil {
				return err
			}
			var out bytes.Buffer
			switch target {
			case "zsh":
				zshInit(&out, comp.Bytes(), alias)
			case "bash":
				bashInit(&out, comp.Bytes(), alias)
			case "fish":
				fishInit(&out, comp.Bytes(), alias)
			}
			_, err = os.Stdout.Write(out.Bytes())
			return err
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "also define `NAME` as an alias for hop")
	return cmd
}

func zshInit(w *bytes.Buffer, comp []byte, alias string) {
	fmt.Fprintln(w, `# hop: eval "$(hop init zsh)" in ~/.zshrc`)
	if alias != "" {
		// A zsh function of the same name would shadow the alias: remove it.
		fmt.Fprintf(w, "if (( $+functions[%s] )); then unfunction -- %s; fi\nalias -- %s=hop\n", alias, alias, alias)
	}
	fmt.Fprint(w, `# compdef only exists once compinit has run; do not break the shell otherwise.
if (( $+functions[compdef] )); then
`)
	w.Write(comp)
	fmt.Fprintln(w, "fi")
}

// bashInit also works without the bash-completion package: cobra's script then falls
// back on __hop_init_completion, which still needs _get_comp_words_by_ref from that
// package. The version below takes the words as bash split them instead, checked at
// each completion since bash-completion may be loaded after hop.
func bashInit(w *bytes.Buffer, comp []byte, alias string) {
	fmt.Fprintln(w, `# hop: eval "$(hop init bash)" in ~/.bashrc`)
	w.Write(comp)
	fmt.Fprint(w, `# Without the bash-completion package: the words as bash split them.
__hop_init_completion()
{
    COMPREPLY=()
    if declare -F _get_comp_words_by_ref >/dev/null 2>&1; then
        _get_comp_words_by_ref "$@" cur prev words cword
        return
    fi
    words=("${COMP_WORDS[@]}")
    cword=$COMP_CWORD
    cur=${COMP_WORDS[COMP_CWORD]}
    prev=
    if (( cword > 0 )); then
        prev=${COMP_WORDS[cword-1]}
    fi
}
`)
	if alias != "" {
		// A function of the same name would shadow the alias: remove it. The completion
		// calls the word typed (the alias), which bash expands to hop.
		fmt.Fprintf(w, `unset -f -- %[1]s 2>/dev/null
alias -- %[1]s=hop
if [[ $(type -t compopt) = "builtin" ]]; then
    complete -o default -F __start_hop %[1]s
else
    complete -o default -o nospace -F __start_hop %[1]s
fi
`, alias)
	}
}

// fishInit defines the alias as fish's own alias command does: a function that wraps
// hop, so it inherits hop's completion. It replaces any function of that name.
func fishInit(w *bytes.Buffer, comp []byte, alias string) {
	fmt.Fprintln(w, `# hop: hop init fish | source, in ~/.config/fish/config.fish`)
	w.Write(comp)
	if alias != "" {
		fmt.Fprintf(w, "function %[1]s --wraps hop --description 'alias %[1]s=hop'\n    hop $argv\nend\n", alias)
	}
}

// unsupportedShell points PowerShell to hop completion, which covers it without the
// alias.
func unsupportedShell(target string) error {
	if target == "powershell" || target == "pwsh" {
		return errors.New("for PowerShell, see hop completion powershell --help")
	}
	return fmt.Errorf("unknown target %q: zsh, bash, fish or ssh (for PowerShell, see hop completion powershell --help)", target)
}
