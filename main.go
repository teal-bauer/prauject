package main

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/teal-bauer/prauject/internal/server"
)

func main() {
	var port int
	var dev bool
	var claudeArgs []string

	root := &cobra.Command{
		Use:   "prauject",
		Short: "Claude Code session manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := server.New(dev, claudeArgs)
			if err != nil {
				return fmt.Errorf("creating server: %w", err)
			}
			addr := fmt.Sprintf(":%d", port)
			log.Printf("prauject listening on http://localhost%s", addr)
			return srv.ListenAndServe(addr)
		},
	}

	root.Flags().IntVarP(&port, "port", "p", 8090, "listen port")
	root.Flags().BoolVar(&dev, "dev", false, "dev mode (reload templates from disk)")
	root.Flags().StringArrayVar(&claudeArgs, "claude-arg", nil, "extra arguments passed to claude on resume (repeatable)")

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
