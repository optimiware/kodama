// Package chatopshttp is the HTTP adapter of the ChatOps module: authenticated
// destination configuration, sanitized delivery inspection and manual replay.
// It reuses the shared router, bounded body reading, strict JSON, quoted-version
// ETags, scoped cursors and error envelopes of internal/httpapi, and holds no
// persistence or business rule: every rule comes from internal/integrations/chatops.
//
// DTOs are generated from api/chatops.yaml (chatops.gen.go) and mapped by dto.go;
// no entity is serialized directly. The router authenticates, throttles and
// authorizes the tenant, project and resource reference before a handler runs
// (see Rules), and the store repeats the exact permission inside each
// operation's transaction.
//
// There is deliberately no route that approves anything, no capability link and
// no webhook receiver: ChatOps only sends notifications whose links name the
// existing authenticated resource GET routes.
package chatopshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// Resource kinds passed to the router's Authorizer.
const (
	ResourceDestination = "chatops_destination"
	ResourceDelivery    = "chatops_delivery"
)

const (
	projectBase  = "/tenants/{tenantId}/projects/{projectId}"
	destinations = projectBase + "/chatops-destinations"
	deliveries   = projectBase + "/chatops-deliveries"
	destSort     = "id"
	delSort      = "created_at desc,id desc"
)

// RouteInfo describes one route for the route inventory.
type RouteInfo struct {
	Method string
	// Path is relative to the router's "/api/v1" prefix.
	Path   string
	Action httpapi.Action
}

// Routes lists the module's routes: the exported inventory the OpenAPI contract
// test compares with api/chatops.yaml.
func Routes() []RouteInfo {
	return []RouteInfo{
		{http.MethodGet, destinations, ActionDestinationList},
		{http.MethodPost, destinations, ActionDestinationCreate},
		{http.MethodGet, destinations + "/{id}", ActionDestinationRead},
		{http.MethodPatch, destinations + "/{id}", ActionDestinationUpdate},
		{http.MethodGet, deliveries, ActionDeliveryList},
		{http.MethodGet, deliveries + "/{id}", ActionDeliveryRead},
		{http.MethodPost, deliveries + "/{id}/replay", ActionDeliveryReplay},
	}
}

// Service is the consumer-owned port of the handlers; *chatops.Service satisfies it.
type Service interface {
	CreateDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, in chatops.DestinationInput) (chatops.Destination, error)
	GetDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID) (chatops.Destination, error)
	ListDestinations(ctx context.Context, c chatops.Caller, scope domain.Scope, after chatops.DestinationID, limit int) ([]chatops.Destination, error)
	UpdateDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID, expected int64, edit chatops.DestinationEdit) (chatops.Destination, error)
	ListDeliveries(ctx context.Context, c chatops.Caller, scope domain.Scope, f chatops.DeliveryFilter, after chatops.DeliveryPosition, limit int) ([]chatops.Delivery, error)
	GetDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID) (chatops.DeliveryView, error)
	ReplayDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID, expected int64, reason string) (chatops.Delivery, error)
}

var _ Service = (*chatops.Service)(nil)

// Handler serves the routes.
type Handler struct {
	svc   Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("chatopshttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

// RegisterRoutes registers the routes on a router whose prefix is "/api/v1".
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	handlers := map[string]httpapi.Handler{
		http.MethodGet + " " + destinations:                 h.listDestinations,
		http.MethodPost + " " + destinations:                h.createDestination,
		http.MethodGet + " " + destinations + "/{id}":       h.getDestination,
		http.MethodPatch + " " + destinations + "/{id}":     h.updateDestination,
		http.MethodGet + " " + deliveries:                   h.listDeliveries,
		http.MethodGet + " " + deliveries + "/{id}":         h.getDelivery,
		http.MethodPost + " " + deliveries + "/{id}/replay": h.replayDelivery,
	}
	for _, info := range Routes() {
		route := httpapi.Route{Method: info.Method, Path: info.Path, Action: info.Action, Handler: handlers[info.Method+" "+info.Path]}
		switch {
		case strings.HasPrefix(info.Path, destinations+"/"):
			route.ResourceKind, route.ResourceParam = ResourceDestination, "id"
		case strings.HasPrefix(info.Path, deliveries+"/"):
			route.ResourceKind, route.ResourceParam = ResourceDelivery, "id"
		}
		if err := rt.Handle(route); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) chatops.Caller {
	// The principal is the router's verified identity, never a body field.
	return chatops.Caller{Actor: chatops.Actor{Type: chatops.ActorUser, ID: c.Principal.ID}, RequestID: c.RequestID}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

func (h *Handler) scope(w http.ResponseWriter, r *http.Request, c *httpapi.Call) (domain.Scope, bool) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return domain.Scope{}, false
	}
	return scope, true
}

func writeWithETag(w http.ResponseWriter, r *http.Request, status int, v int64, body any, location string) {
	if err := httpapi.SetETag(w, version(v)); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	if location != "" {
		w.Header().Set("Location", location)
	}
	httpapi.WriteJSON(w, r, status, body)
}

// knownEvents reports whether every type is on the module's allow-list, as the
// schema's enum states; the entity checks the same rule again.
func knownEvents(in []EventType) bool {
	for _, e := range in {
		if !chatops.IsSupportedEvent(string(e)) {
			return false
		}
	}
	return true
}

func invalidCursor() error {
	return httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request")
}

// ---- destinations ------------------------------------------------------------

type destinationPosition struct {
	ID string `json:"id"`
}

func (h *Handler) listDestinations(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(map[string][]string{}, destSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var after chatops.DestinationID
	if page.Position != nil {
		var pos destinationPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || chatops.DestinationID(pos.ID).Validate() != nil {
			h.fail(w, r, invalidCursor())
			return
		}
		after = chatops.DestinationID(pos.ID)
	}
	rows, err := h.svc.ListDestinations(r.Context(), h.caller(c), scope, after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Destination, 0, len(rows))
	for _, d := range rows {
		items = append(items, destinationDTO(d))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, items, func(d Destination) ([]byte, error) {
		return json.Marshal(destinationPosition{ID: string(d.Id)})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) createDestination(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req DestinationCreate
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Name == "" || req.Endpoint == "" || len(req.Events) == 0 || !knownEvents(req.Events) {
		h.fail(w, r, httpapi.BadRequestError("a destination needs a name, an endpoint and at least one event type"))
		return
	}
	d, err := h.svc.CreateDestination(r.Context(), h.caller(c), scope, destinationInput(req))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusCreated, d.Version, destinationDTO(d), strings.TrimSuffix(r.URL.Path, "/")+"/"+string(d.ID))
}

func (h *Handler) getDestination(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	d, err := h.svc.GetDestination(r.Context(), h.caller(c), scope, chatops.DestinationID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, d.Version, destinationDTO(d), "")
}

func (h *Handler) updateDestination(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req DestinationEdit
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Events != nil && (len(*req.Events) == 0 || !knownEvents(*req.Events)) {
		h.fail(w, r, httpapi.BadRequestError("a destination selects at least one event type"))
		return
	}
	edit := destinationEdit(req)
	if edit.Empty() {
		h.fail(w, r, httpapi.BadRequestError("an edit changes at least one member"))
		return
	}
	d, err := h.svc.UpdateDestination(r.Context(), h.caller(c), scope, chatops.DestinationID(r.PathValue("id")), int64(expected), edit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, d.Version, destinationDTO(d), "")
}

// ---- deliveries --------------------------------------------------------------

type deliveryPosition struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

func (h *Handler) listDeliveries(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r, "state", "destinationId"); err != nil {
		h.fail(w, r, err)
		return
	}
	var f chatops.DeliveryFilter
	filters := map[string][]string{}
	q := r.URL.Query()
	for name, set := range map[string]func(string){
		"state":         func(v string) { f.State = chatops.DeliveryState(v) },
		"destinationId": func(v string) { f.Destination = chatops.DestinationID(v) },
	} {
		vals := q[name]
		if len(vals) == 0 {
			continue
		}
		if len(vals) != 1 || vals[0] == "" {
			h.fail(w, r, httpapi.BadRequestError(name+" must be given once"))
			return
		}
		set(vals[0])
		filters[name] = []string{vals[0]}
	}
	cs := c.CursorScope(filters, delSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var after chatops.DeliveryPosition
	if page.Position != nil {
		var pos deliveryPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil {
			h.fail(w, r, invalidCursor())
			return
		}
		t, terr := time.Parse(time.RFC3339Nano, pos.T)
		if terr != nil || chatops.DeliveryID(pos.ID).Validate() != nil {
			h.fail(w, r, invalidCursor())
			return
		}
		after = chatops.DeliveryPosition{CreatedAt: t, ID: chatops.DeliveryID(pos.ID)}
	}
	rows, err := h.svc.ListDeliveries(r.Context(), h.caller(c), scope, f, after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The position needs the creation time, which the DTO carries.
	items := make([]Delivery, 0, len(rows))
	for _, d := range rows {
		items = append(items, deliveryDTO(d))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, items, func(d Delivery) ([]byte, error) {
		return json.Marshal(deliveryPosition{T: d.CreatedAt.UTC().Format(time.RFC3339Nano), ID: string(d.Id)})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) getDelivery(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	v, err := h.svc.GetDelivery(r.Context(), h.caller(c), scope, chatops.DeliveryID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, v.Delivery.Version, detailDTO(v), "")
}

func (h *Handler) replayDelivery(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req ReplayRequest
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.Reason) == "" || len(req.Reason) > chatops.MaxReasonBytes {
		h.fail(w, r, httpapi.BadRequestError("a replay carries a reason of 1 to 512 bytes"))
		return
	}
	d, err := h.svc.ReplayDelivery(r.Context(), h.caller(c), scope, chatops.DeliveryID(r.PathValue("id")), int64(expected), req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, d.Version, deliveryDTO(d), "")
}
