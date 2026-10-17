// traffic_http.go：GET /traffic?name=&kind=host|forward&seconds= 返回某对象最近的流量曲线。
package wgtund

import (
	"net/http"
	"strconv"
)

func (h *trafficHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "" && kind != "host" && kind != "forward" {
		errJSON(w, http.StatusBadRequest, "kind 只能是 host 或 forward")
		return
	}
	secs := trafficKeep
	if s := q.Get("seconds"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			errJSON(w, http.StatusBadRequest, "seconds 要是正整数")
			return
		}
		if n < secs {
			secs = n
		}
	}
	name := q.Get("name")
	kind, c := h.find(kind, name)
	if c == nil {
		errJSON(w, http.StatusNotFound, "未知主机或转发")
		return
	}
	writeJSON(w, struct {
		Name   string         `json:"name"`
		Kind   string         `json:"kind"`
		Points []trafficPoint `json:"points"`
	}{name, kind, c.points(secs)})
}
