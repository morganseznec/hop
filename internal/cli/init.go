package cli

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"
)

// aliasName keeps --alias to names that are safe to print into a shell script.
var aliasName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func newInit() *cobra.Command {
	var alias string
	cmd := &cobra.Command{
		Use:   "init zsh [--alias NAME]",
		Short: "zsh integration code: completion and an optional alias",
		Long: `Put this in ~/.zshrc, after compinit:

    eval "$(hop init zsh)"

--alias NAME also makes NAME an alias for hop, and removes any zsh function of that
name that would shadow it. For people used to typing another name:

    eval "$(hop init zsh --alias ssm)"`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "zsh" {
				return fmt.Errorf("only zsh is supported")
			}
			if alias != "" && !aliasName.MatchString(alias) {
				return fmt.Errorf("invalid alias name %q: use only letters, digits, _ and -", alias)
			}
			var comp bytes.Buffer
			if err := cmd.Root().GenZshCompletion(&comp); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, `# hop: eval "$(hop init zsh)" in ~/.zshrc`)
			if alias != "" {
				// A zsh function of the same name would shadow the alias: remove it.
				fmt.Fprintf(os.Stdout, "if (( $+functions[%s] )); then unfunction -- %s; fi\nalias -- %s=hop\n", alias, alias, alias)
			}
			fmt.Fprint(os.Stdout, `# compdef only exists once compinit has run; do not break the shell otherwise.
if (( $+functions[compdef] )); then
`)
			os.Stdout.Write(comp.Bytes())
			fmt.Fprintln(os.Stdout, "fi")
			return nil
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "also define `NAME` as an alias for hop")
	return cmd
}
