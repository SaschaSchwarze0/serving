package service

import (
	"context"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
)

var (
	kserviceWritesByNamespaceLimiter                  = map[string]*rate.Limiter{}
	kserviceWritesByNamespaceLimiterMutex             sync.Mutex
	kserviceWritesByNamespaceMaxPerTimeSlice          = -1
	kserviceWritesByNamespaceTimeSliceDurationSeconds = 0.0
	kserviceWritesByNamespaceWaitTime                 = 5 * time.Second
	kserviceWritesUnlimitedNamespaces                 = []string{}
)

func init() {
	var err error
	if value, found := os.LookupEnv("KSERVICE_WRITES_BY_NAMESPACE_TIME_SLICE_DURATION_SECONDS"); found {
		if kserviceWritesByNamespaceTimeSliceDurationSeconds, err = strconv.ParseFloat(value, 64); err != nil {
			log.Fatalf("Non-numeric value for KSERVICE_WRITES_BY_NAMESPACE_TIME_SLICE_DURATION_SECONDS: %s", value)
		}
	}

	if value, found := os.LookupEnv("KSERVICE_WRITES_BY_NAMESPACE_MAX_PER_TIME_SLICE"); found {
		if kserviceWritesByNamespaceMaxPerTimeSlice, err = strconv.Atoi(value); err != nil {
			log.Fatalf("Non-numeric value for KSERVICE_WRITES_BY_NAMESPACE_MAX_PER_TIME_SLICE: %s", value)
		}
	}

	if value, found := os.LookupEnv("KSERVICE_WRITES_BY_NAMESPACE_WAIT_TIME_SECONDS"); found {
		var kserviceWritesByNamespaceWaitTimeSeconds int
		if kserviceWritesByNamespaceWaitTimeSeconds, err = strconv.Atoi(value); err != nil {
			log.Fatalf("Non-numeric value for KSERVICE_WRITES_BY_NAMESPACE_WAIT_TIME_SECONDS: %s", value)
		}
		kserviceWritesByNamespaceWaitTime = time.Duration(kserviceWritesByNamespaceWaitTimeSeconds) * time.Second
	}

	if value := os.Getenv("KSERVICE_WRITES_UNLIMITED_NAMESPACES"); len(value) > 0 {
		kserviceWritesUnlimitedNamespaces = strings.Split(value, ",")
	}
}

func rateLimit(ctx context.Context, namespace string) error {
	// check if rate limiting is enabled
	if kserviceWritesByNamespaceMaxPerTimeSlice < 0 {
		return nil
	}

	// check if the namespace is unlimited
	if slices.Contains(kserviceWritesUnlimitedNamespaces, namespace) {
		return nil
	}

	kserviceWritesByNamespaceLimiterMutex.Lock()

	limiter := kserviceWritesByNamespaceLimiter[namespace]
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(float64(kserviceWritesByNamespaceMaxPerTimeSlice)/kserviceWritesByNamespaceTimeSliceDurationSeconds), kserviceWritesByNamespaceMaxPerTimeSlice)
		kserviceWritesByNamespaceLimiter[namespace] = limiter
	}

	kserviceWritesByNamespaceLimiterMutex.Unlock()

	if !limiter.Allow() {
		logger := logging.FromContext(ctx)
		logger.Infof("Rate-limiting write operation originated from service in namespace %s", namespace)

		return controller.NewRequeueAfter(kserviceWritesByNamespaceWaitTime)
	}

	return nil
}
