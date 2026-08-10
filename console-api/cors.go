package main

import "github.com/go-chi/cors"

// corsOptions returns the API's CORS policy. The wildcard origin is only safe
// because the API authenticates with the Authorization bearer header and never
// with cookies, so no origin can ride a user's ambient credentials. If auth
// ever moves to cookies, AllowCredentials must become true — and a wildcard
// origin paired with credentials is both refused by browsers and a serious
// hole. corsOptionsAreSafe guards that invariant (see cors_test.go).
func corsOptions() cors.Options {
	return cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}
}

// corsOptionsAreSafe reports whether a CORS policy avoids the wildcard-origin +
// credentials combination, which would let any site make credentialed requests.
func corsOptionsAreSafe(o cors.Options) bool {
	if !o.AllowCredentials {
		return true
	}
	for _, origin := range o.AllowedOrigins {
		if origin == "*" {
			return false
		}
	}
	return true
}
