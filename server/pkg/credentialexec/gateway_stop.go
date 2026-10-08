package credentialexec

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func readGatewayStop(state string) error {
	path := filepath.Join(state, "gateway-stop")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16 {
		return ErrUnavailable
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return ErrUnavailable
	}
	switch string(contents) {
	case "quota\n":
		return ErrQuotaExhausted
	case "unknown\n":
		return ErrOutcomeUnknown
	default:
		return ErrUnavailable
	}
}

func (boundary *Boundary) stopErrorLocked() error {
	if boundary.stopErr == nil {
		boundary.stopErr = readGatewayStop(boundary.state)
		if boundary.stopErr != nil && boundary.stopped != nil {
			close(boundary.stopped)
		}
	}
	return boundary.stopErr
}

func (boundary *Boundary) StopError() error {
	if boundary == nil {
		return nil
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	return boundary.stopErrorLocked()
}

func (boundary *Boundary) Stopped() <-chan struct{} {
	if boundary == nil {
		return nil
	}
	return boundary.stopped
}

func (boundary *Boundary) stopGateway(reason error) {
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if boundary.stopErrorLocked() != nil {
		return
	}
	boundary.stopErr = reason
	defer close(boundary.stopped)
	contents := "unknown\n"
	if errors.Is(reason, ErrQuotaExhausted) {
		contents = "quota\n"
	}
	file, err := os.OpenFile(filepath.Join(boundary.state, "gateway-stop"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		boundary.stopErr = ErrUnavailable
		return
	}
	_, writeErr := io.WriteString(file, contents)
	syncErr := file.Sync()
	closeErr := file.Close()
	directory, err := os.Open(boundary.state)
	if err != nil {
		boundary.stopErr = ErrUnavailable
		return
	}
	directorySyncErr := directory.Sync()
	directoryCloseErr := directory.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || directorySyncErr != nil || directoryCloseErr != nil {
		boundary.stopErr = ErrUnavailable
	}
}

type gatewayTransport struct {
	boundary  *Boundary
	transport *http.Transport
}

func stoppedGatewayResponse(request *http.Request, err error) *http.Response {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrQuotaExhausted) {
		status = http.StatusTooManyRequests
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Cache-Control": []string{"no-store"}}, Body: io.NopCloser(strings.NewReader(err.Error())), Request: request}
}

func (transport *gatewayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.boundary.requestMutex.Lock()
	defer transport.boundary.requestMutex.Unlock()
	if err := transport.boundary.StopError(); err != nil {
		return stoppedGatewayResponse(request, err), nil
	}
	response, err := transport.transport.RoundTrip(request)
	if err != nil || response.StatusCode >= http.StatusMultipleChoices {
		reason := ErrOutcomeUnknown
		if response != nil {
			if response.StatusCode == http.StatusTooManyRequests {
				reason = ErrQuotaExhausted
			}
			_ = response.Body.Close()
		}
		transport.boundary.stopGateway(reason)
		return stoppedGatewayResponse(request, transport.boundary.StopError()), nil
	}
	return response, nil
}
