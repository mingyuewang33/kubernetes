package remotecommand

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/exec"
	"k8s.io/klog/v2"
)

var (
	streamIdleTimeout     = 4 * time.Hour
	streamCreationTimeout = remotecommandconsts.DefaultStreamCreationTimeout
)

// proxyStreamToWebSocket proxies stream to url with websocket.
func ProxyToWebSocket(w http.ResponseWriter, r *http.Request, url *url.URL, opts *Options) {
	klog.V(8).Infof("start proxy request to websocket %v", r)
	ctx, ok := createStreams(
		r,
		w,
		opts,
		remotecommandconsts.SupportedStreamingProtocols,
		streamIdleTimeout,
		streamCreationTimeout)
	if !ok {
		msg := "failed to create stream to fontend"
		klog.Error(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := ctx.conn.Close(); err != nil {
			klog.Errorf("failed to close connection, %v", err)
		}
	}()

	klog.V(8).Infof("start connecting to websocket %s", url.String())
	backendConn, err := connectBackend(url.String(), "channel.k8s.io", r)
	if err != nil {
		msg := fmt.Sprintf("connectBackend failed: %v", err)
		klog.Error(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	defer backendConn.Close()

	var errConnection error
	frontendStdinToBackendComplete := make(chan struct{})
	frontendResizeToBackendComplete := make(chan struct{})
	backendToFrontendComplete := make(chan struct{})

	go func() {
		for {
			_, msg, err := backendConn.ReadMessage()
			if err != nil {
				e, ok := err.(*websocket.CloseError)
				if !ok || e.Code != websocket.CloseNormalClosure {
					errConnection = err
				}
				break
			}

			if len(msg) < 1 {
				errConnection = fmt.Errorf("received err msg from backEnd (the length less than 1), msg: %s", string(msg))
				break
			}

			switch msg[0] {
			case stdoutChannel:
				_, err = ctx.stdoutStream.Write(msg[1:])
			case stderrChannel:
				_, err = ctx.stderrStream.Write(msg[1:])
			case errorChannel:
				err = ctx.writeStatus(apierrors.NewInternalError(errors.New(string(msg[1:]))))
			default:
				err = fmt.Errorf("received invalid msg from backEnd, msg: %s", string(msg))
			}

			if err != nil {
				errConnection = err
				break
			}
		}
		close(backendToFrontendComplete)
	}()

	if opts.Stdin {
		go func() {
			r := &rwc{
				c:     backendConn,
				index: stdinChannel,
			}
			_, err := io.Copy(r, ctx.stdinStream)
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				errConnection = fmt.Errorf("copy data from frontend(stdinStream) to backend failed, err: %v", err)
			}
			close(frontendStdinToBackendComplete)
		}()
	}

	if opts.TTY {
		go func() {
			r := &rwc{
				c:     backendConn,
				index: resizeChannel,
			}
			_, err := io.Copy(r, ctx.resizeStream)
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				errConnection = fmt.Errorf("copy data from frontend(resizeStream) to backend failed, err: %v", err)
			}
			close(frontendResizeToBackendComplete)
		}()
	}
	select {
	case <-backendToFrontendComplete:
	case <-frontendStdinToBackendComplete:
	case <-frontendResizeToBackendComplete:
	}

	if errConnection != nil {
		klog.Errorf("SpdyProxy: the connection disconnected: %v", errConnection)
		if exitErr, ok := errConnection.(exec.ExitError); ok && exitErr.Exited() {
			rc := exitErr.ExitStatus()
			ctx.writeStatus(&apierrors.StatusError{ErrStatus: v1.Status{
				Status: v1.StatusFailure,
				Reason: remotecommandconsts.NonZeroExitCodeReason,
				Details: &v1.StatusDetails{
					Causes: []v1.StatusCause{
						{
							Type:    remotecommandconsts.ExitCodeCauseType,
							Message: fmt.Sprintf("%d", rc),
						},
					},
				},
				Message: fmt.Sprintf("command terminated with non-zero exit code: %v", exitErr),
			}})
		} else if closeErr, ok := errConnection.(*websocket.CloseError); !ok || closeErr.Text != io.ErrUnexpectedEOF.Error() {
			//ignore this ErrUnexpectedEOF because isulad always close the connection while reading a frame
			err = fmt.Errorf("error executing command in container: %v", errConnection)
			runtime.HandleError(err)
			ctx.writeStatus(apierrors.NewInternalError(err))
		}
	} else {
		ctx.writeStatus(&apierrors.StatusError{ErrStatus: v1.Status{
			Status: v1.StatusSuccess,
		}})
	}
}

func connectBackend(addr, subprotocol string, r *http.Request) (*websocket.Conn, error) {
	h := http.Header{}
	originHeadValue := r.Header.Get("Origin")
	if originHeadValue != "" {
		h["Origin"] = []string{originHeadValue}
	}
	websocket.DefaultDialer.Subprotocols = []string{subprotocol}
	websocket.DefaultDialer.ReadBufferSize = 128 * 1024
	websocket.DefaultDialer.WriteBufferSize = 128 * 1024
	ws, resp, err := websocket.DefaultDialer.Dial(addr, h)
	if err == nil {
		return ws, nil
	}
	msg := fmt.Errorf("dial failed: %v, response Body is nil", err)
	if resp != nil && resp.Body != nil {
		defer func() {
			//websocket buffer size maybe not enough and cause panic
			if e := recover(); e != nil {
				msg = fmt.Errorf("dial failed: %v, response panic %v", err, e)
			}
			resp.Body.Close()
		}()
		var body bytes.Buffer
		body.ReadFrom(resp.Body)
		msg = fmt.Errorf("dial failed: %v, response is: %v", err, body.String())
	}
	return nil, msg
}

type rwc struct {
	c     *websocket.Conn
	index byte
}

func (c *rwc) Write(p []byte) (int, error) {
	frame := make([]byte, len(p)+1)
	frame[0] = byte(c.index)
	copy(frame[1:], p)

	err := c.c.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
