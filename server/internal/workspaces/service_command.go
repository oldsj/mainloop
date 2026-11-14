package workspaces

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/oldsj/mainloop/server/internal/api"
)

// Bounds of the command kagent accepts (Workspace.service_command).
const (
	maxServiceArgs   = 64
	maxServiceArgLen = 4096
)

// serviceCommand is the workspace service command sent to kagent: the project's declared argv
// run through env(1) with the workspace's identity and preview URLs, so the service can set its
// dev origin. MAINLOOP_PREVIEW_URLS lists `<port>=<url>` per declared port, by port number,
// separated by commas. The agent reads the same values from the command in the service's status
// file. It is computed once, when the workspace is planned, and stored, so every replay of the
// create sends the same request. An empty declaration stays empty.
func serviceCommand(declared []string, previewBase, workspaceID string, ports []api.WorkspacePort) ([]string, error) {
	if len(declared) == 0 {
		return []string{}, nil
	}
	numbers := make([]int, 0, len(ports))
	for _, p := range ports {
		numbers = append(numbers, p.Number)
	}
	slices.Sort(numbers)
	urls := make([]string, 0, len(numbers))
	for _, n := range numbers {
		urls = append(urls, strconv.Itoa(n)+"="+previewURL(previewBase, workspaceID, n))
	}
	command := slices.Concat([]string{
		"env", "MAINLOOP_WORKSPACE_ID=" + workspaceID, "MAINLOOP_PREVIEW_URLS=" + strings.Join(urls, ","),
	}, declared)
	if len(command) > maxServiceArgs || slices.ContainsFunc(command, func(arg string) bool { return len(arg) > maxServiceArgLen }) {
		return nil, fmt.Errorf("the workspace service command with its preview URLs is over kagent's limit of %d arguments of %d bytes", maxServiceArgs, maxServiceArgLen)
	}
	return command, nil
}
