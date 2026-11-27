package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/esquery"
	"patchman-be/internal/models"
)

func TestKibanaScopeParam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &models.ESConnection{OptionsJSON: `{"kibanaSpace":"ops","kibanaTenant":"merchant"}`}
	cases := []struct{ q, space, tenant string }{
		{"", "ops", "merchant"}, // configured defaults
		{"?tenant=", "ops", ""}, // explicit empty = OSD default
		{"?tenant=Global&space=default", "", "global"},
		{"?tenant=__user__", "ops", "private"},
	}
	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/x"+tc.q, nil)
		sc, err := kibanaScopeParam(c, conn)
		if err != nil || sc.Space != tc.space || sc.Tenant != tc.tenant {
			t.Fatalf("%q: got %+v %v; want space %q tenant %q", tc.q, sc, err, tc.space, tc.tenant)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/x?tenant=a%0Ab", nil)
	if _, err := kibanaScopeParam(c, conn); err == nil {
		t.Fatalf("control chars must be rejected")
	}
	if got := scopeTarget(esquery.KibanaScope{Space: "ops", Tenant: "merchant"}); got != "ops|t=merchant" {
		t.Fatalf("scopeTarget: %q", got)
	}
}
