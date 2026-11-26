package management

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registerbot"
	log "github.com/sirupsen/logrus"
)

// The engine is initialized on first use; ordinary CPA startup does not launch Chrome.
func (h *Handler) registerBotEngine() (*registerbot.Engine, error) {
	h.registerBotMu.Lock()
	defer h.registerBotMu.Unlock()
	if h.registerBotClosed {
		return nil, errors.New("registerbot: server is stopping")
	}
	if h.registerBot != nil {
		return h.registerBot, nil
	}
	if h.configFilePath == "" {
		return nil, errors.New("registerbot: config file is required")
	}
	dir := filepath.Join(filepath.Dir(h.configFilePath), "registerbot")
	settings := registerbot.DefaultSettings()
	settings.StateDir = dir
	engine, errNew := registerbot.NewEngine(dir, settings, h.importRegisterBotKey)
	if errNew != nil {
		return nil, errNew
	}
	h.registerBot = engine
	return engine, nil
}

// CloseRegisterBot releases owned workers and browsers during server shutdown.
func (h *Handler) CloseRegisterBot() error {
	h.registerBotMu.Lock()
	h.registerBotClosed = true
	engine := h.registerBot
	h.registerBotMu.Unlock()
	if engine != nil {
		return engine.Close()
	}
	return nil
}

func (h *Handler) requireRegisterBot(c *gin.Context) *registerbot.Engine {
	c.Header("Cache-Control", "no-store")
	engine, errInit := h.registerBotEngine()
	if errInit != nil {
		log.WithField("component", "registerbot").Warn("registration engine initialization failed")
		registerBotError(c, http.StatusServiceUnavailable, "engine_unavailable")
		return nil
	}
	return engine
}

func registerBotError(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": code}})
}

func decodeRegisterBot(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 300*1024)
	decoder := json.NewDecoder(c.Request.Body)
	if errDecode := decoder.Decode(dst); errDecode != nil {
		registerBotError(c, http.StatusBadRequest, "invalid_request")
		return false
	}
	var extra any
	if errTrailing := decoder.Decode(&extra); errTrailing != io.EOF {
		registerBotError(c, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (h *Handler) GetRegisterBot(c *gin.Context) {
	if engine := h.requireRegisterBot(c); engine != nil {
		c.JSON(http.StatusOK, engine.Snapshot())
	}
}

func (h *Handler) GetRegisterBotChannels(c *gin.Context) {
	if engine := h.requireRegisterBot(c); engine != nil {
		c.JSON(http.StatusOK, gin.H{"channels": engine.Settings().Channels})
	}
}

func (h *Handler) PutRegisterBotChannel(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	var channel registerbot.ChannelSettings
	if !decodeRegisterBot(c, &channel) {
		return
	}
	if channel.ID != c.Param("id") || channel.ID == "" {
		registerBotError(c, http.StatusBadRequest, "channel_mismatch")
		return
	}
	settings := engine.Settings()
	found := false
	for i := range settings.Channels {
		if settings.Channels[i].ID == channel.ID {
			settings.Channels[i] = channel
			found = true
			break
		}
	}
	if !found {
		if len(settings.Channels) >= 32 {
			registerBotError(c, http.StatusBadRequest, "channel_limit")
			return
		}
		settings.Channels = append(settings.Channels, channel)
	}
	if errUpdate := engine.UpdateSettings(settings); errUpdate != nil {
		registerBotError(c, http.StatusConflict, "settings_update_failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"channels": engine.Settings().Channels})
}

func (h *Handler) StartRegisterBotRun(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	var request registerbot.StartRequest
	if !decodeRegisterBot(c, &request) {
		return
	}
	run, errStart := engine.Start(request)
	if errStart != nil {
		registerBotError(c, http.StatusBadRequest, "run_rejected")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"run": run})
}

func (h *Handler) RegisterBotAction(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	var request struct {
		Action string `json:"action"`
	}
	if !decodeRegisterBot(c, &request) {
		return
	}
	if errAction := engine.Action(c.Param("run"), request.Action); errAction != nil {
		registerBotError(c, http.StatusConflict, "action_rejected")
		return
	}
	c.JSON(http.StatusOK, engine.Snapshot())
}

func (h *Handler) RegisterBotSubmitKey(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	var request struct {
		RowID string `json:"row_id"`
		Key   string `json:"key"`
	}
	if !decodeRegisterBot(c, &request) {
		return
	}
	if errSubmit := engine.SubmitKey(c.Param("run"), request.RowID, request.Key); errSubmit != nil {
		registerBotError(c, http.StatusConflict, "key_rejected")
		return
	}
	c.JSON(http.StatusAccepted, engine.Snapshot())
}

func (h *Handler) GetRegisterBotGateway(c *gin.Context) {
	if engine := h.requireRegisterBot(c); engine != nil {
		c.JSON(http.StatusOK, engine.Gateway().Snapshot())
	}
}

func (h *Handler) ProbeRegisterBotGateway(c *gin.Context) {
	if engine := h.requireRegisterBot(c); engine != nil {
		status := engine.Gateway().Probe(c.Request.Context(), true)
		engine.Events().Emit(registerbot.Event{Stage: "gateway_probe", Level: "info", Message: status.Mode})
		c.JSON(http.StatusOK, status)
	}
}

func (h *Handler) GetRegisterBotProxyPool(c *gin.Context) {
	if engine := h.requireRegisterBot(c); engine != nil {
		c.JSON(http.StatusOK, engine.ProxyPoolSnapshot())
	}
}

func (h *Handler) PutRegisterBotProxyPool(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	var settings registerbot.ProxyPoolSettings
	if !decodeRegisterBot(c, &settings) {
		return
	}
	if errUpdate := engine.UpdateProxyPool(settings); errUpdate != nil {
		registerBotError(c, http.StatusConflict, "proxy_pool_update_failed")
		return
	}
	engine.Events().Emit(registerbot.Event{Stage: "proxy_pool_updated", Level: "info", Message: "proxy_pool_updated"})
	c.JSON(http.StatusOK, engine.ProxyPoolSnapshot())
}

func (h *Handler) RefreshRegisterBotProxyPool(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	snapshot, errRefresh := engine.RefreshProxyPool(c.Request.Context())
	if errRefresh != nil {
		engine.Events().Emit(registerbot.Event{Stage: "proxy_pool_unavailable", Level: "warning", Message: "proxy_pool_unavailable"})
		registerBotError(c, http.StatusServiceUnavailable, "proxy_pool_refresh_failed")
		return
	}
	engine.Events().Emit(registerbot.Event{Stage: "proxy_pool_refreshed", Level: "info", Message: "proxy_pool_refreshed"})
	c.JSON(http.StatusOK, snapshot)
}

// RegisterBotEvents streams authenticated cursor replay without query-string credentials.
func (h *Handler) RegisterBotEvents(c *gin.Context) {
	engine := h.requireRegisterBot(c)
	if engine == nil {
		return
	}
	raw := c.Query("after")
	if raw == "" {
		raw = c.GetHeader("Last-Event-ID")
	}
	after := int64(0)
	if raw != "" {
		var errCursor error
		after, errCursor = strconv.ParseInt(raw, 10, 64)
		if errCursor != nil || after < 0 {
			registerBotError(c, http.StatusBadRequest, "invalid_cursor")
			return
		}
	}
	channel := strings.TrimSpace(c.Query("channel"))
	replay, live, cancel, reset := engine.Events().SubscribeAfter(after, channel, 128)
	defer cancel()
	c.Header("Content-Type", "text/event-stream")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	write := func(kind string, seq int64, value any) bool {
		data, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return false
		}
		prefix := ""
		if seq > 0 {
			prefix = fmt.Sprintf("id: %d\n", seq)
		}
		if _, errWrite := fmt.Fprintf(c.Writer, "%sevent: %s\ndata: %s\n\n", prefix, kind, data); errWrite != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
	if reset {
		after = 0
		if !write("reset", 0, gin.H{"cursor": 0}) {
			return
		}
	}
	for _, event := range replay {
		if !write("registerbot", event.Seq, event) {
			return
		}
		after = event.Seq
	}
	if !write("ready", 0, gin.H{"cursor": after}) {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, open := <-live:
			if !open {
				return
			}
			if event.Seq <= after || (channel != "" && event.Channel != channel) {
				continue
			}
			if !write("registerbot", event.Seq, event) {
				return
			}
			after = event.Seq
		case <-ticker.C:
			if _, errWrite := io.WriteString(c.Writer, ": keepalive\n\n"); errWrite != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}
