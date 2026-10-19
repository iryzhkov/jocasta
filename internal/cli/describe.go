package cli

import (
	"encoding/json"
	"io"
)

// writeDescription uses the same one-line purposes as full help, without
// parsing configuration, reading credentials, making requests or spooling.
func writeDescription(out io.Writer) int {
	type command struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
		Access  string `json:"access"`
	}
	description := struct {
		Name     string    `json:"name"`
		Summary  string    `json:"summary"`
		Help     string    `json:"help"`
		Commands []command `json:"commands"`
	}{Name: "jocasta", Summary: "Shared working documents with revision-guarded writes.", Help: "jocasta --help"}
	for _, topic := range helpTopics {
		access, summary := "write", topic.purpose
		switch topic.verb {
		case "ls", "search", "history", "version":
			access = "read"
		case "get", "exists", "diff":
			summary += "; records read activity"
		}
		description.Commands = append(description.Commands, command{topic.verb, summary, access})
	}
	description.Commands = append(description.Commands, command{"help", "Show command help", "read"})
	if json.NewEncoder(out).Encode(description) != nil {
		return ExitIO
	}
	return ExitOK
}

func descriptionRequested(args []string) bool {
	return len(args) == 1 && args[0] == "--describe"
}
