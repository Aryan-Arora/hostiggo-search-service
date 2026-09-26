package handler

import (
	"net/http"

	"github.com/Aryan-Arora/search-service-backend/internal/vercelapi"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	vercelapi.Handler(w, r)
}
