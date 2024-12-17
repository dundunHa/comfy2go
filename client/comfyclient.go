package client

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dundunHa/comfy2go/graphapi"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"encoding/base64"
)

type QueuedItemStoppedReason string

const (
	QueuedItemStoppedReasonFinished    QueuedItemStoppedReason = "finished"
	QueuedItemStoppedReasonInterrupted QueuedItemStoppedReason = "interrupted"
	QueuedItemStoppedReasonError       QueuedItemStoppedReason = "error"
)

type ComfyClientCallbacks struct {
	ClientQueueCountChanged func(*ComfyClient, int)
	QueuedItemStarted       func(*ComfyClient, *QueueItem)
	QueuedItemStopped       func(*ComfyClient, *QueueItem, QueuedItemStoppedReason)
	QueuedItemDataAvailable func(*ComfyClient, *QueueItem, *PromptMessageData)
}

type ComfyClientOptions struct {
	UseHttps  bool          // 是否使用 HTTPS/WSS
	Timeout   int           // 连接超时时间（秒）
	MaxRetry  int           // 最大重试次数
	BaseDelay time.Duration // 基础重试延迟
	MaxDelay  time.Duration // 最大重��延迟
	Auth      *Auth
}

// Auth represents authentication credentials
type Auth struct {
	Username string
	Password string
	Token    string
}

// ComfyClient is the top level object that allows for interaction with the ComfyUI backend
type ComfyClient struct {
	serverBaseAddress     string
	serverAddress         string
	serverPort            int
	clientid              string
	webSocket             *WebSocketConnection
	nodeobjects           *graphapi.NodeObjects
	initialized           bool
	queueditems           map[string]*QueueItem
	queuecount            int
	callbacks             *ComfyClientCallbacks
	lastProcessedPromptID string
	timeout               int
	httpclient            *http.Client
	httpProtocol          string
	auth                  *Auth
}

// DefaultComfyClientOptions returns the default options for a ComfyClient
func DefaultComfyClientOptions() *ComfyClientOptions {
	return &ComfyClientOptions{
		UseHttps:  false,
		Timeout:   -1,
		MaxRetry:  5,
		BaseDelay: 1 * time.Second,
		MaxDelay:  10 * time.Second,
	}
}

// NewComfyClientWithTimeout creates a new instance of a Comfy2go client with a connection timeout
func NewComfyClientWithTimeout(server_address string, server_port int, callbacks *ComfyClientCallbacks, timeout int, retry int) *ComfyClient {
	options := DefaultComfyClientOptions()
	options.Timeout = timeout
	options.MaxRetry = retry
	return NewComfyClientWithOptions(server_address, server_port, callbacks, options)
}

// NewComfyClient creates a new instance of a Comfy2go client
func NewComfyClient(server_address string, server_port int, callbacks *ComfyClientCallbacks) *ComfyClient {
	return NewComfyClientWithOptions(server_address, server_port, callbacks, DefaultComfyClientOptions())
}

// NewComfyClientWithOptions creates a new instance of a Comfy2go client with options
func NewComfyClientWithOptions(server_address string, server_port int, callbacks *ComfyClientCallbacks, options *ComfyClientOptions) *ComfyClient {
	protocol := "ws"
	if options.UseHttps {
		protocol = "wss"
	}
	sbaseaddr := server_address
	if server_port > 0 {
		sbaseaddr = server_address + ":" + strconv.Itoa(server_port)
	}
	httpProtocol := "http"
	if options.UseHttps {
		httpProtocol = "https"
	}
	cid := uuid.New().String()

	// 创建一个新的dialer并设置header
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 45 * time.Second,
	}

	// 如果配置了认证,创建header
	var header http.Header
	if options.Auth != nil {
		header = http.Header{
			"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(options.Auth.Username+":"+options.Auth.Password))},
		}
	}

	retv := &ComfyClient{
		serverBaseAddress: sbaseaddr,
		serverAddress:     server_address,
		serverPort:        server_port,
		clientid:          cid,
		queueditems:       make(map[string]*QueueItem),
		webSocket: &WebSocketConnection{
			WebSocketURL:   protocol + "://" + sbaseaddr + "/ws?clientId=" + cid,
			ConnectionDone: make(chan bool),
			MaxRetry:       options.MaxRetry,
			ManagerStarted: false,
			BaseDelay:      options.BaseDelay,
			MaxDelay:       options.MaxDelay,
			Dialer:        dialer,
			Header:        header,
		},
		initialized: false,
		queuecount:  0,
		callbacks:   callbacks,
		timeout:     options.Timeout,
		httpclient:  &http.Client{},
		httpProtocol: httpProtocol,
		auth:        options.Auth,
	}
	// golang uses mark-sweep GC, so this circular reference should be fine
	retv.webSocket.Callback = retv

	return retv
}

func (cc *ComfyClient) SetDialer(dialer *websocket.Dialer) {
	// dereference the pointer and copy the values
	cc.webSocket.Dialer = *dialer
}

func (cc *ComfyClient) OnMessage(message string) {
	cc.OnWindowSocketMessage(message)
}

// IsInitialized returns true if the client's websocket is connected and initialized
func (c *ComfyClient) IsInitialized() bool {
	if c.initialized {
		// ping the websocket to see if it is still connected
		err := c.webSocket.Ping()
		if err != nil {
			c.webSocket.Conn.Close()
			c.initialized = false
			c.webSocket.IsConnected = false
		}
	}
	return c.initialized
}

// CheckConnection checks if the websocket connection is still active and tries to reinitialize if not
func (c *ComfyClient) CheckConnection() error {
	if !c.IsInitialized() {
		// try to initialize first
		err := c.Init()
		if err != nil {
			return err
		}
	}
	return nil
}

// Init starts the websocket connection (if not already connected) and retrieves the collection of node objects
func (c *ComfyClient) Init() error {
	if !c.webSocket.IsConnected {
		// as soon as the ws is connected, it will receive a "status" message of the current status
		// of the ComfyUI server
		err := c.webSocket.ConnectWithManager(c.timeout)
		if err != nil {
			return err
		}
	}

	// Get the object infos for the Comfy Server
	object_infos, err := c.GetObjectInfos()
	if err != nil {
		return err
	}

	c.nodeobjects = object_infos
	c.initialized = true
	return nil
}

// ClientID returns the unique client ID for the connection to the ComfyUI backend
func (c *ComfyClient) ClientID() string {
	return c.clientid
}

// return the underlying http client
func (c *ComfyClient) HttpClient() *http.Client {
	return c.httpclient
}

// set the underlying http client
func (c *ComfyClient) SetHttpClient(client *http.Client) {
	c.httpclient = client
}

// NewGraphFromJsonReader creates a new graph from the data read from an io.Reader
func (c *ComfyClient) NewGraphFromJsonReader(r io.Reader) (*graphapi.Graph, *[]string, error) {
	if !c.IsInitialized() {
		// try to initialize first
		err := c.Init()
		if err != nil {
			return nil, nil, err
		}
	}
	return graphapi.NewGraphFromJsonReader(r, c.nodeobjects)
}

// NewGraphFromJsonFile creates a new graph from a JSON file
func (c *ComfyClient) NewGraphFromJsonFile(path string) (*graphapi.Graph, *[]string, error) {
	if !c.IsInitialized() {
		// try to initialize first
		err := c.Init()
		if err != nil {
			return nil, nil, err
		}
	}
	return graphapi.NewGraphFromJsonFile(path, c.nodeobjects)
}

// NewGraphFromJsonString creates a new graph from a JSON string
func (c *ComfyClient) NewGraphFromJsonString(path string) (*graphapi.Graph, *[]string, error) {
	if !c.IsInitialized() {
		// try to initialize first
		err := c.Init()
		if err != nil {
			return nil, nil, err
		}
	}
	return graphapi.NewGraphFromJsonString(path, c.nodeobjects)
}

// NewGraphFromPNGReader extracts the workflow from PNG data read from an io.Reader and creates a new graph
func (c *ComfyClient) NewGraphFromPNGReader(r io.Reader) (*graphapi.Graph, *[]string, error) {
	metadata, err := GetPngMetadata(r)
	if err != nil {
		return nil, nil, err
	}

	// get the workflow from the PNG metadata
	workflow, ok := metadata["workflow"]
	if !ok {
		return nil, nil, errors.New("png does not contain workflow metadata")
	}
	reader := strings.NewReader(workflow)

	graph, missing, err := c.NewGraphFromJsonReader(reader)
	if err != nil {
		return nil, missing, err
	}
	return graph, missing, nil
}

// NewGraphFromPNGReader extracts the workflow from PNG data read from a file and creates a new graph
func (c *ComfyClient) NewGraphFromPNGFile(path string) (*graphapi.Graph, *[]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	return c.NewGraphFromPNGReader(file)
}

// GetQueuedItem returns a QueueItem that was queued with the ComfyClient, that has not been processed yet
// or is currently being processed.  Once a QueueItem has been processed, it will not be available with this method.
func (c *ComfyClient) GetQueuedItem(prompt_id string) *QueueItem {
	val, ok := c.queueditems[prompt_id]
	if ok {
		return val
	}
	return nil
}

// OnWindowSocketMessage processes each message received from the websocket connection to ComfyUI.
// The messages are parsed, and translated into PromptMessage structs and placed into the correct QueuedItem's message channel.
func (c *ComfyClient) OnWindowSocketMessage(msg string) {
	message := &WSStatusMessage{}
	err := json.Unmarshal([]byte(msg), &message)
	if err != nil {
		slog.Error("Deserializing Status Message:", err)
	}

	switch message.Type {
	case "status":
		s := message.Data.(*WSMessageDataStatus)
		if c.callbacks != nil && c.callbacks.ClientQueueCountChanged != nil {
			c.queuecount = s.Status.ExecInfo.QueueRemaining
			c.callbacks.ClientQueueCountChanged(c, s.Status.ExecInfo.QueueRemaining)
		}
	case "execution_start":
		s := message.Data.(*WSMessageDataExecutionStart)
		qi := c.GetQueuedItem(s.PromptID)
		// update lastProcessedPromptID to indicate we are processing a new prompt
		c.lastProcessedPromptID = s.PromptID
		if qi != nil {
			if c.callbacks != nil && c.callbacks.QueuedItemStarted != nil {
				c.callbacks.QueuedItemStarted(c, qi)
			}
			m := PromptMessage{
				Type: "started",
				Message: &PromptMessageStarted{
					PromptID: qi.PromptID,
				},
			}
			qi.Messages <- m
		}
	case "execution_cached":
		// this is probably not usefull for us
	case "executing":
		s := message.Data.(*WSMessageDataExecuting)
		qi := c.GetQueuedItem(s.PromptID)

		if qi != nil {
			if s.Node == nil {
				// final node was processed
				m := PromptMessage{
					Type: "stopped",
					Message: &PromptMessageStopped{
						QueueItem: qi,
						Exception: nil,
					},
				}
				// remove the Item from our Queue before sending the message
				// no other messages will be sent to the channel after this
				if c.callbacks != nil && c.callbacks.QueuedItemStopped != nil {
					c.callbacks.QueuedItemStopped(c, qi, QueuedItemStoppedReasonFinished)
				}
				delete(c.queueditems, qi.PromptID)
				qi.Messages <- m
			} else {
				node := qi.Workflow.GetNodeById(*s.Node)
				m := PromptMessage{
					Type: "executing",
					Message: &PromptMessageExecuting{
						NodeID: *s.Node,
						Title:  node.DisplayName,
					},
				}
				qi.Messages <- m
			}
		}
	case "progress":
		s := message.Data.(*WSMessageDataProgress)
		qi := c.GetQueuedItem(c.lastProcessedPromptID)
		if qi != nil {
			m := PromptMessage{
				Type: "progress",
				Message: &PromptMessageProgress{
					Value: s.Value,
					Max:   s.Max,
				},
			}
			qi.Messages <- m
		}
	case "executed":
		s := message.Data.(*WSMessageDataExecuted)
		qi := c.GetQueuedItem(s.PromptID)
		if qi != nil {
			// mdata := &PromptMessageData{
			// 	NodeID: s.Node,
			// 	Images: *s.Output["images"],
			// }

			// collect the data from the output
			mdata := &PromptMessageData{
				NodeID: s.Node,
				Data:   make(map[string][]DataOutput),
			}

			for k, v := range s.Output {
				mdata.Data[k] = *v
			}

			m := PromptMessage{
				Type:    "data",
				Message: mdata,
			}
			if c.callbacks != nil && c.callbacks.QueuedItemDataAvailable != nil {
				c.callbacks.QueuedItemDataAvailable(c, qi, mdata)
			}
			qi.Messages <- m
		}
	case "execution_interrupted":
		s := message.Data.(*WSMessageExecutionInterrupted)
		qi := c.GetQueuedItem(s.PromptID)
		if qi != nil {
			m := PromptMessage{
				Type: "stopped",
				Message: &PromptMessageStopped{
					QueueItem: qi,
					Exception: nil,
				},
			}
			// remove the Item from our Queue before sending the message
			// no other messages will be sent to the channel after this
			if c.callbacks != nil && c.callbacks.QueuedItemStopped != nil {
				c.callbacks.QueuedItemStopped(c, qi, QueuedItemStoppedReasonInterrupted)
			}
			delete(c.queueditems, qi.PromptID)
			qi.Messages <- m
		}
	case "execution_error":
		s := message.Data.(*WSMessageExecutionError)
		qi := c.GetQueuedItem(s.PromptID)
		if qi != nil {
			nindex, _ := strconv.Atoi(s.Node) // the node id is serialized as a string
			tnode := qi.Workflow.GetNodeById(nindex)
			m := PromptMessage{
				Type: "stopped",
				Message: &PromptMessageStopped{
					QueueItem: qi,
					Exception: &PromptMessageStoppedException{
						NodeID:           nindex,
						NodeType:         s.NodeType,
						NodeName:         tnode.Title,
						ExceptionMessage: s.ExceptionMessage,
						ExceptionType:    s.ExceptionType,
						Traceback:        s.Traceback,
					},
				},
			}
			// remove the Item from our Queue before sending the message
			// no other messages will be sent to the channel after this
			if c.callbacks != nil && c.callbacks.QueuedItemStopped != nil {
				c.callbacks.QueuedItemStopped(c, qi, QueuedItemStoppedReasonError)
			}
			delete(c.queueditems, qi.PromptID)
			qi.Messages <- m
		}
	default:
		// Handle unknown data types or return a dedicated error here
		slog.Warn("Unhandled message type: ", "type", message.Type)
	}
}

// getAuthHeader returns the Basic auth header value if auth is configured
func (c *ComfyClient) getAuthHeader() string {
	if c.auth == nil {
		return ""
	}

	if c.auth.Token != "" {
		return base64.StdEncoding.EncodeToString([]byte(c.auth.Token + ":"))
	}

	if c.auth.Username != "" {
		return base64.StdEncoding.EncodeToString([]byte(c.auth.Username + ":" + c.auth.Password))
	}

	return ""
}
