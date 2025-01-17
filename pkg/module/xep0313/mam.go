// Copyright 2022 The jackal Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xep0313

import (
	"context"
	"errors"
	"time"

	kitlog "github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/jackal-xmpp/stravaganza"
	stanzaerror "github.com/jackal-xmpp/stravaganza/errors/stanza"
	"github.com/jackal-xmpp/stravaganza/jid"
	"github.com/ortuman/jackal/pkg/hook"
	"github.com/ortuman/jackal/pkg/host"
	archivemodel "github.com/ortuman/jackal/pkg/model/archive"
	c2smodel "github.com/ortuman/jackal/pkg/model/c2s"
	"github.com/ortuman/jackal/pkg/module/xep0004"
	"github.com/ortuman/jackal/pkg/module/xep0059"
	"github.com/ortuman/jackal/pkg/router"
	"github.com/ortuman/jackal/pkg/storage/repository"
	xmpputil "github.com/ortuman/jackal/pkg/util/xmpp"
	"github.com/samber/lo"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// ModuleName represents mam module name.
	ModuleName = "mam"

	// XEPNumber represents mam XEP number.
	XEPNumber = "0313"

	mamNamespace         = "urn:xmpp:mam:2"
	extendedMamNamespace = "urn:xmpp:mam:2#extended"

	dateTimeFormat = "2006-01-02T15:04:05Z"

	archiveRequestedCtxKey = "mam:requested"

	defaultPageSize = 50
	maxPageSize     = 250
)

type archiveIDCtxKey int

const (
	sentArchiveIDKey archiveIDCtxKey = iota
	receivedArchiveIDKey
)

// Config contains mam module configuration options.
type Config struct {
	// QueueSize defines maximum number of archive messages stanzas.
	// When the limit is reached, the oldest message will be purged to make room for the new one.
	QueueSize int `fig:"queue_size" default:"1000"`
}

// Mam represents a mam (XEP-0313) module type.
type Mam struct {
	cfg    Config
	hosts  hosts
	router router.Router
	hk     *hook.Hooks
	rep    repository.Repository
	logger kitlog.Logger
}

// New returns a new initialized mam instance.
func New(
	cfg Config,
	router router.Router,
	hosts *host.Hosts,
	rep repository.Repository,
	hk *hook.Hooks,
	logger kitlog.Logger,
) *Mam {
	return &Mam{
		cfg:    cfg,
		router: router,
		hosts:  hosts,
		rep:    rep,
		hk:     hk,
		logger: kitlog.With(logger, "module", ModuleName, "xep", XEPNumber),
	}
}

// Name returns mam module name.
func (m *Mam) Name() string { return ModuleName }

// StreamFeature returns mam module stream feature.
func (m *Mam) StreamFeature(_ context.Context, _ string) (stravaganza.Element, error) {
	return nil, nil
}

// ServerFeatures returns mam server disco features.
func (m *Mam) ServerFeatures(_ context.Context) ([]string, error) {
	return nil, nil
}

// AccountFeatures returns mam account disco features.
func (m *Mam) AccountFeatures(_ context.Context) ([]string, error) {
	return []string{mamNamespace, extendedMamNamespace}, nil
}

// Start starts mam module.
func (m *Mam) Start(_ context.Context) error {
	m.hk.AddHook(hook.C2SStreamMessageReceived, m.onMessageReceived, hook.HighestPriority)
	m.hk.AddHook(hook.S2SInStreamMessageReceived, m.onMessageReceived, hook.HighestPriority)

	m.hk.AddHook(hook.C2SStreamMessageRouted, m.onMessageRouted, hook.LowestPriority+2)
	m.hk.AddHook(hook.S2SInStreamMessageRouted, m.onMessageRouted, hook.LowestPriority+2)
	m.hk.AddHook(hook.UserDeleted, m.onUserDeleted, hook.DefaultPriority)

	level.Info(m.logger).Log("msg", "started mam module")
	return nil
}

// Stop stops mam module.
func (m *Mam) Stop(_ context.Context) error {
	m.hk.RemoveHook(hook.C2SStreamMessageReceived, m.onMessageReceived)
	m.hk.RemoveHook(hook.S2SInStreamMessageReceived, m.onMessageReceived)
	m.hk.RemoveHook(hook.C2SStreamMessageRouted, m.onMessageRouted)
	m.hk.RemoveHook(hook.S2SInStreamMessageRouted, m.onMessageRouted)
	m.hk.RemoveHook(hook.UserDeleted, m.onUserDeleted)

	level.Info(m.logger).Log("msg", "stopped mam module")
	return nil
}

// MatchesNamespace tells whether namespace matches mam module.
func (m *Mam) MatchesNamespace(namespace string, serverTarget bool) bool {
	if serverTarget {
		return false
	}
	return namespace == mamNamespace
}

// ProcessIQ process a mam iq.
func (m *Mam) ProcessIQ(ctx context.Context, iq *stravaganza.IQ) error {
	fromJID := iq.FromJID()
	toJID := iq.ToJID()

	if !fromJID.MatchesWithOptions(toJID, jid.MatchesBare) {
		_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.Forbidden))
		return nil
	}
	switch {
	case iq.IsGet() && iq.ChildNamespace("metadata", mamNamespace) != nil:
		return m.sendArchiveMetadata(ctx, iq)

	case iq.IsGet() && iq.ChildNamespace("query", mamNamespace) != nil:
		return m.sendFormFields(ctx, iq)

	case iq.IsSet() && iq.ChildNamespace("query", mamNamespace) != nil:
		return m.sendArchiveMessages(ctx, iq)
	}
	return nil
}

func (m *Mam) sendArchiveMetadata(ctx context.Context, iq *stravaganza.IQ) error {
	archiveID := iq.FromJID().Node()

	metadata, err := m.rep.FetchArchiveMetadata(ctx, archiveID)
	if err != nil {
		_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.InternalServerError))
		return err
	}
	// send reply
	metadataBuilder := stravaganza.NewBuilder("metadata").WithAttribute(stravaganza.Namespace, mamNamespace)

	startBuilder := stravaganza.NewBuilder("start")
	if metadata != nil {
		startBuilder.WithAttribute("id", metadata.StartId)
		startBuilder.WithAttribute("timestamp", metadata.StartTimestamp)
	}
	endBuilder := stravaganza.NewBuilder("end")
	if metadata != nil {
		endBuilder.WithAttribute("id", metadata.EndId)
		endBuilder.WithAttribute("timestamp", metadata.EndTimestamp)
	}

	metadataBuilder.WithChildren(startBuilder.Build(), endBuilder.Build())

	resIQ := xmpputil.MakeResultIQ(iq, metadataBuilder.Build())
	_, _ = m.router.Route(ctx, resIQ)

	level.Info(m.logger).Log("msg", "requested archive metadata", "archive_id", archiveID)

	return nil
}

func (m *Mam) sendFormFields(ctx context.Context, iq *stravaganza.IQ) error {
	form := xep0004.DataForm{
		Type: xep0004.Form,
	}

	form.Fields = append(form.Fields, xep0004.Field{
		Type:   xep0004.Hidden,
		Var:    xep0004.FormType,
		Values: []string{mamNamespace},
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.JidSingle,
		Var:  "with",
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.TextSingle,
		Var:  "start",
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.TextSingle,
		Var:  "end",
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.TextSingle,
		Var:  "before-id",
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.TextSingle,
		Var:  "after-id",
	})
	form.Fields = append(form.Fields, xep0004.Field{
		Type: xep0004.ListMulti,
		Var:  "ids",
		Validate: &xep0004.Validate{
			DataType:  xep0004.StringDataType,
			Validator: &xep0004.OpenValidator{},
		},
	})

	qChild := stravaganza.NewBuilder("query").
		WithAttribute(stravaganza.Namespace, mamNamespace).
		WithChild(form.Element()).
		Build()

	_, _ = m.router.Route(ctx, xmpputil.MakeResultIQ(iq, qChild))

	level.Info(m.logger).Log("msg", "requested form fields")

	return nil
}

func (m *Mam) sendArchiveMessages(ctx context.Context, iq *stravaganza.IQ) error {
	fromJID := iq.FromJID()

	stm, err := m.router.C2S().LocalStream(fromJID.Node(), fromJID.Resource())
	if err != nil {
		return err
	}

	qChild := iq.ChildNamespace("query", mamNamespace)

	// filter archive result
	filters := &archivemodel.Filters{}
	if x := qChild.ChildNamespace("x", xep0004.FormNamespace); x != nil {
		form, err := xep0004.NewFormFromElement(x)
		if err != nil {
			return err
		}
		filters, err = formToFilters(form)
		if err != nil {
			return err
		}
	}
	archiveID := fromJID.Node()

	messages, err := m.rep.FetchArchiveMessages(ctx, filters, archiveID)
	if err != nil {
		_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.InternalServerError))
		return err
	}
	// run archive queried event
	if err := m.runHook(ctx, hook.ArchiveMessageQueried, &hook.MamInfo{
		ArchiveID: archiveID,
		Filters:   filters,
	}); err != nil {
		return err
	}

	// return not found error if any requested id cannot be found
	switch {
	case len(filters.Ids) > 0 && (len(messages) != len(filters.Ids)):
		fallthrough

	case (len(filters.AfterId) > 0 || len(filters.BeforeId) > 0) && len(messages) == 0:
		_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.ItemNotFound))
		return nil
	}

	// apply RSM paging
	var req *xep0059.Request
	var res *xep0059.Result

	if set := qChild.ChildNamespace("set", xep0059.RSMNamespace); set != nil {
		req, err = xep0059.NewRequestFromElement(set)
		if err != nil {
			_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.BadRequest))
			return err
		}
		if req.Max > maxPageSize {
			req.Max = maxPageSize
		}
	} else {
		req = &xep0059.Request{Max: defaultPageSize}
	}
	messages, res, err = xep0059.GetResultSetPage(messages, req, func(m *archivemodel.Message) string {
		return m.Id
	})
	if err != nil {
		if errors.Is(err, xep0059.ErrPageNotFound) {
			_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.ItemNotFound))
			return nil
		}
		_, _ = m.router.Route(ctx, xmpputil.MakeErrorStanza(iq, stanzaerror.InternalServerError))
		return err
	}

	// flip result page
	if qChild.Child("flip-page") != nil {
		messages = lo.Reverse(messages)

		lastID := res.Last
		res.Last = res.First
		res.First = lastID
	}

	// route archive messages
	for _, msg := range messages {
		msgStanza, _ := stravaganza.NewBuilderFromProto(msg.Message).
			BuildStanza()
		stamp := msg.Stamp.AsTime()

		resultElem := stravaganza.NewBuilder("result").
			WithAttribute(stravaganza.Namespace, mamNamespace).
			WithAttribute("queryid", qChild.Attribute("queryid")).
			WithAttribute(stravaganza.ID, uuid.New().String()).
			WithChild(xmpputil.MakeForwardedStanza(msgStanza, &stamp)).
			Build()

		archiveMsg, _ := stravaganza.NewMessageBuilder().
			WithAttribute(stravaganza.From, iq.ToJID().String()).
			WithAttribute(stravaganza.To, iq.FromJID().String()).
			WithAttribute(stravaganza.ID, uuid.New().String()).
			WithChild(resultElem).
			BuildMessage()

		_, _ = m.router.Route(ctx, archiveMsg)
	}

	finB := stravaganza.NewBuilder("fin").
		WithChild(res.Element()).
		WithAttribute(stravaganza.Namespace, mamNamespace)
	if res.Complete {
		finB.WithAttribute("complete", "true")
	}
	_, _ = m.router.Route(ctx, xmpputil.MakeResultIQ(iq, finB.Build()))

	level.Info(m.logger).Log("msg", "archive messages requested", "archive_id", fromJID.Node(), "count", len(messages), "complete", res.Complete)

	return stm.SetInfoValue(ctx, archiveRequestedCtxKey, true)
}

func (m *Mam) onMessageReceived(execCtx *hook.ExecutionContext) error {
	var msg *stravaganza.Message

	switch inf := execCtx.Info.(type) {
	case *hook.C2SStreamInfo:
		msg = inf.Element.(*stravaganza.Message)
		inf.Element = m.addRecipientStanzaID(msg)
		execCtx.Info = inf

	case *hook.S2SStreamInfo:
		msg = inf.Element.(*stravaganza.Message)
		inf.Element = m.addRecipientStanzaID(msg)
		execCtx.Info = inf
	}
	return nil
}

func (m *Mam) onMessageRouted(execCtx *hook.ExecutionContext) error {
	var elem stravaganza.Element

	switch inf := execCtx.Info.(type) {
	case *hook.C2SStreamInfo:
		elem = inf.Element
	case *hook.S2SStreamInfo:
		elem = inf.Element
	}
	return m.handleRoutedMessage(execCtx, elem)
}

func (m *Mam) onUserDeleted(execCtx *hook.ExecutionContext) error {
	inf := execCtx.Info.(*hook.UserInfo)
	return m.rep.DeleteArchive(execCtx.Context, inf.Username)
}

func (m *Mam) handleRoutedMessage(execCtx *hook.ExecutionContext, elem stravaganza.Element) error {
	msg, ok := elem.(*stravaganza.Message)
	if !ok {
		return nil
	}
	if !isMessageArchievable(msg) {
		return nil
	}

	fromJID := msg.FromJID()
	if m.hosts.IsLocalHost(fromJID.Domain()) {
		sentArchiveID := uuid.New().String()
		archiveMsg := xmpputil.MakeStanzaIDMessage(msg, sentArchiveID, fromJID.ToBareJID().String())
		if err := m.archiveMessage(execCtx.Context, archiveMsg, fromJID.Node(), sentArchiveID); err != nil {
			return err
		}
		execCtx.Context = context.WithValue(execCtx.Context, sentArchiveIDKey, sentArchiveID)
	}
	toJID := msg.ToJID()
	if !m.hosts.IsLocalHost(toJID.Domain()) {
		return nil
	}
	recievedArchiveID := xmpputil.MessageStanzaID(msg)
	if err := m.archiveMessage(execCtx.Context, msg, toJID.Node(), recievedArchiveID); err != nil {
		return err
	}
	execCtx.Context = context.WithValue(execCtx.Context, receivedArchiveIDKey, recievedArchiveID)
	return nil
}

func (m *Mam) archiveMessage(ctx context.Context, message *stravaganza.Message, archiveID, id string) error {
	archiveMsg := &archivemodel.Message{
		ArchiveId: archiveID,
		Id:        id,
		FromJid:   message.FromJID().String(),
		ToJid:     message.ToJID().String(),
		Message:   message.Proto(),
		Stamp:     timestamppb.Now(),
	}
	err := m.rep.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
		err := tx.InsertArchiveMessage(ctx, archiveMsg)
		if err != nil {
			return err
		}
		return tx.DeleteArchiveOldestMessages(ctx, archiveID, m.cfg.QueueSize)
	})
	if err != nil {
		return err
	}
	return m.runHook(ctx, hook.ArchiveMessageArchived, &hook.MamInfo{
		ArchiveID: archiveID,
		Message:   archiveMsg,
	})
}

func (m *Mam) addRecipientStanzaID(originalMsg *stravaganza.Message) *stravaganza.Message {
	toJID := originalMsg.ToJID()
	if !m.hosts.IsLocalHost(toJID.Domain()) {
		return originalMsg
	}
	archiveID := uuid.New().String()
	return xmpputil.MakeStanzaIDMessage(originalMsg, archiveID, toJID.ToBareJID().String())
}

func (m *Mam) runHook(ctx context.Context, hookName string, inf *hook.MamInfo) error {
	_, err := m.hk.Run(hookName, &hook.ExecutionContext{
		Info:    inf,
		Sender:  m,
		Context: ctx,
	})
	return err
}

// IsArchiveRequested determines whether archive has been requested over a C2S stream by inspecting inf parameter.
func IsArchiveRequested(inf c2smodel.Info) bool {
	return inf.Bool(archiveRequestedCtxKey)
}

// ExtractSentArchiveID returns message sent archive ID by inspecting the passed context.
func ExtractSentArchiveID(ctx context.Context) string {
	ret, ok := ctx.Value(sentArchiveIDKey).(string)
	if ok {
		return ret
	}
	return ""
}

// ExtractReceivedArchiveID returns message received archive ID by inspecting the passed context.
func ExtractReceivedArchiveID(ctx context.Context) string {
	ret, ok := ctx.Value(receivedArchiveIDKey).(string)
	if ok {
		return ret
	}
	return ""
}

func formToFilters(fm *xep0004.DataForm) (*archivemodel.Filters, error) {
	var retVal archivemodel.Filters

	fmType := fm.Fields.ValueForFieldOfType(xep0004.FormType, xep0004.Hidden)
	if fm.Type != xep0004.Submit || fmType != mamNamespace {
		return nil, errors.New("unexpected form type value")
	}
	if start := fm.Fields.ValueForField("start"); len(start) > 0 {
		startTm, err := time.Parse(dateTimeFormat, start)
		if err != nil {
			return nil, err
		}
		retVal.Start = timestamppb.New(startTm)
	}
	if end := fm.Fields.ValueForField("end"); len(end) > 0 {
		endTm, err := time.Parse(dateTimeFormat, end)
		if err != nil {
			return nil, err
		}
		retVal.End = timestamppb.New(endTm)
	}
	if with := fm.Fields.ValueForField("with"); len(with) > 0 {
		retVal.With = with
	}
	if beforeID := fm.Fields.ValueForField("before-id"); len(beforeID) > 0 {
		retVal.BeforeId = beforeID
	}
	if afterID := fm.Fields.ValueForField("after-id"); len(afterID) > 0 {
		retVal.AfterId = afterID
	}
	if ids := fm.Fields.ValuesForField("ids"); len(ids) > 0 {
		retVal.Ids = ids
	}
	return &retVal, nil
}

func isMessageArchievable(msg *stravaganza.Message) bool {
	return (msg.IsNormal() || msg.IsChat()) && msg.IsMessageWithBody()
}
