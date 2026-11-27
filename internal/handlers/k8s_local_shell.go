package handlers

import (
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"k8s.io/client-go/tools/clientcmd"
)

// LocalShell opens an interactive login shell ON THE USER'S OWN MACHINE with
// KUBECONFIG pointed at the selected connection/context, so they run
// kubectl/aws/gcloud locally already aimed at the cluster (Lens-style). This is
// DIFFERENT from Exec, which streams a shell inside a pod.
//
// GET /k8s/:id/local-shell?context=
//
// SECURITY GATE (critical): spawning a local login shell is powerful and must
// ONLY happen on the user's own machine. The handler refuses with 403 unless
// the process runs in embedded (desktop sidecar) mode — on a hosted/shared
// server BE this would spawn a shell on the server, which is never allowed.
// The write grant (access(c,true)) is required on top of that, matching Exec.
//
// The kubeconfig is written to a 0600 temp file, removed on disconnect; the
// child shell is killed when the WebSocket closes. Framing is OUR protocol,
// identical to Exec: the FE sends {type:stdin|resize} and hears
// {type:stdout|exit}.
func (h *K8sOpsHandler) LocalShell(c *gin.Context) {
	// Gate 1: embedded mode only. Refuse before any auth work so a hosted BE
	// gives a clear, uniform answer.
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "local shell is only available on the desktop app"})
		return
	}
	// Gate 2: write grant, same as Exec.
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}

	contextName := c.Query("context")
	if strings.TrimSpace(contextName) == "" {
		contextName = strings.TrimSpace(conn.DefaultContext)
	}

	// Decrypt + parse the stored kubeconfig (no cluster call). Exec blocks are
	// stripped inside the loader (RCE guard on what PatchMan writes).
	kcfg, err := h.registry.LoadKubeconfigForShell(*conn)
	if err != nil {
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Validate the requested context exists before we set it — a bogus context
	// would otherwise silently write a kubeconfig with a dangling current-context.
	if strings.TrimSpace(contextName) == "" {
		err = errBadRequest("no context specified and connection has no default context")
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, exists := kcfg.Contexts[contextName]; !exists {
		err = errBadRequest("context " + contextName + " not found in kubeconfig")
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	kcfg.CurrentContext = contextName

	// Write the kubeconfig to a private temp file the shell inherits via
	// KUBECONFIG. CreateTemp already makes it 0600; Chmod is belt-and-suspenders.
	f, err := os.CreateTemp("", "patchman-kubeconfig-*.yaml")
	if err != nil {
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create temp kubeconfig: " + err.Error()})
		return
	}
	kubeconfigPath := f.Name()
	_ = f.Chmod(0o600)
	_ = f.Close()
	// Remove the temp kubeconfig no matter how we leave this handler.
	defer func() { _ = os.Remove(kubeconfigPath) }()
	if err := clientcmd.WriteToFile(*kcfg, kubeconfigPath); err != nil {
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write temp kubeconfig: " + err.Error()})
		return
	}

	// Pick the user's login shell, falling back to common shells.
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = firstExisting("/bin/zsh", "/bin/bash", "/bin/sh")
	}
	if shell == "" {
		err = errBadRequest("no shell available on this host")
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Login shell so the user's PATH/aliases/completions load, with KUBECONFIG
	// overriding whatever the profile might set.
	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		h.audit(c, conn, role, "local_shell", contextName, "", "", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "start shell: " + err.Error()})
		return
	}

	// Audit the START before the long-lived stream (matches Exec).
	h.audit(c, conn, role, "local_shell", contextName, "", "", nil)

	bridgePTY(c, cmd, ptmx)
}

// firstExisting returns the first path that exists on disk, or "".
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// errBadRequest is a tiny error type so the validation branches share one
// string between the audit row and the JSON body.
type errBadRequest string

func (e errBadRequest) Error() string { return string(e) }
