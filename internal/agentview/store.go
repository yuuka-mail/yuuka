package agentview

import (
	"slices"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// InputOf is the Input for message m as the store returned it to viewer
// v, with raw the message of m.Deliveries[0], its earliest visible
// delivery (§13.8: raw and auth come from a path v may read). Every
// shell builds views through here, so the MCP tools and the REST API
// withhold the same delivery facts from a viewer without delivery:read.
func InputOf(v *auth.Viewer, m store.MessageDetail, raw []byte) Input {
	in := Input{
		ID: m.ID, Raw: raw, ReceivedAt: m.ReceivedAt,
		Class: m.Class.String(), Downgraded: m.Downgraded,
		WithholdDelivery: !v.Has(auth.DeliveryRead),
	}
	for _, d := range m.Deliveries {
		if !slices.Contains(in.DeliveredTo, d.EnvelopeTo) {
			in.DeliveredTo = append(in.DeliveredTo, d.EnvelopeTo)
		}
	}
	if len(m.Deliveries) > 0 && m.Deliveries[0].Auth != nil {
		in.Auth = *m.Deliveries[0].Auth
	}
	return in
}
