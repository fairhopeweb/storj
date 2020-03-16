// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package heldamount

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/pb"
	"storj.io/common/rpc"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/storagenode/trust"
)

var (
	// ErrHeldAmountService defines held amount service error
	ErrHeldAmountService = errs.Class("heldamount service error")

	mon = monkit.Package()
)

// Client encapsulates HeldAmountClient with underlying connection
//
// architecture: Client
type Client struct {
	conn *rpc.Conn
	pb.DRPCHeldAmountClient
}

// Close closes underlying client connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// TODO: separate service on service and endpoint.

// Service retrieves info from satellites using an rpc client
//
// architecture: Service
type Service struct {
	log *zap.Logger

	db DB

	dialer rpc.Dialer
	trust  *trust.Pool
}

// NewService creates new instance of service
func NewService(log *zap.Logger, db DB, dialer rpc.Dialer, trust *trust.Pool) *Service {
	return &Service{
		log:    log,
		db:     db,
		dialer: dialer,
		trust:  trust,
	}
}

// GetPaystubStats retrieves held amount for particular satellite from satellite using grpc.
func (service *Service) GetPaystubStats(ctx context.Context, satelliteID storj.NodeID, period string) (_ *PayStub, err error) {
	defer mon.Task()(&ctx)(&err)

	client, err := service.dial(ctx, satelliteID)
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}
	defer func() { err = errs.Combine(err, client.Close()) }()

	requestedPeriod, err := stringToTime(period)
	if err != nil {
		service.log.Error("stringToTime", zap.Error(err))
		return nil, ErrHeldAmountService.Wrap(err)
	}

	resp, err := client.GetPayStub(ctx, &pb.GetHeldAmountRequest{Period: requestedPeriod})
	if err != nil {
		service.log.Error("GetPayStub", zap.Error(err))
		return nil, ErrHeldAmountService.Wrap(err)
	}
	service.log.Error("paystub = = = =", zap.Any("", resp))
	return &PayStub{
		Period:         period,
		SatelliteID:    satelliteID,
		Created:        resp.CreatedAt,
		Codes:          resp.Codes,
		UsageAtRest:    float64(resp.UsageAtRest),
		UsageGet:       resp.UsageGet,
		UsagePut:       resp.UsagePut,
		UsageGetRepair: resp.CompGetRepair,
		UsagePutRepair: resp.CompPutRepair,
		UsageGetAudit:  resp.UsageGetAudit,
		CompAtRest:     resp.CompAtRest,
		CompGet:        resp.CompGet,
		CompPut:        resp.CompPut,
		CompGetRepair:  resp.CompGetRepair,
		CompPutRepair:  resp.CompPutRepair,
		CompGetAudit:   resp.CompGetAudit,
		SurgePercent:   resp.SurgePercent,
		Held:           resp.Held,
		Owed:           resp.Owed,
		Disposed:       resp.Disposed,
		Paid:           resp.Paid,
	}, nil
}

// GetPayment retrieves payment data from particular satellite using grpc.
func (service *Service) GetPayment(ctx context.Context, satelliteID storj.NodeID, period string) (_ *Payment, err error) {
	defer mon.Task()(&ctx)(&err)

	client, err := service.dial(ctx, satelliteID)
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}
	defer func() { err = errs.Combine(err, client.Close()) }()

	requestedPeriod, err := stringToTime(period)
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}

	resp, err := client.GetPayment(ctx, &pb.GetPaymentRequest{Period: requestedPeriod})
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}

	return &Payment{
		ID:          resp.Id,
		Created:     resp.CreatedAt,
		SatelliteID: satelliteID,
		Period:      period,
		Amount:      resp.Amount,
		Receipt:     resp.Receipt,
		Notes:       resp.Notes,
	}, nil
}

// SatellitePayStubMonthlyCached retrieves held amount for particular satellite for selected month from storagenode database.
func (service *Service) SatellitePayStubMonthlyCached(ctx context.Context, satelliteID storj.NodeID, period string) (payStub *PayStub, err error) {
	defer mon.Task()(&ctx, &satelliteID, &period)(&err)

	payStub, err = service.db.GetPayStub(ctx, satelliteID, period)
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}

	return payStub, nil
}

// AllPayStubsMonthlyCached retrieves held amount for particular satellite from storagenode database.
func (service *Service) AllPayStubsMonthlyCached(ctx context.Context, period string) (payStubs []PayStub, err error) {
	defer mon.Task()(&ctx, &period)(&err)

	payStubs, err = service.db.AllPayStubs(ctx, period)
	if err != nil {
		return nil, ErrHeldAmountService.Wrap(err)
	}

	return payStubs, nil
}

// SatellitePayStubPeriodCached retrieves held amount for all satellites for selected months from storagenode database.
func (service *Service) SatellitePayStubPeriodCached(ctx context.Context, satelliteID storj.NodeID, periodStart, periodEnd string) (payStubs []*PayStub, err error) {
	defer mon.Task()(&ctx, &satelliteID, &periodStart, &periodEnd)(&err)

	periods, err := parsePeriodRange(periodStart, periodEnd)
	if err != nil {
		return nil, err
	}

	for _, period := range periods {
		payStub, err := service.db.GetPayStub(ctx, satelliteID, period)
		if err != nil {
			if ErrNoPayStubForPeriod.Has(err) {
				continue
			}
			return nil, ErrHeldAmountService.Wrap(err)
		}

		payStubs = append(payStubs, payStub)
	}

	return payStubs, nil
}

// AllPayStubsPeriodCached retrieves held amount for all satellites for selected range of months from storagenode database.
func (service *Service) AllPayStubsPeriodCached(ctx context.Context, periodStart, periodEnd string) (payStubs []PayStub, err error) {
	defer mon.Task()(&ctx, &periodStart, &periodEnd)(&err)

	periods, err := parsePeriodRange(periodStart, periodEnd)
	if err != nil {
		return nil, err
	}

	for _, period := range periods {
		payStub, err := service.db.AllPayStubs(ctx, period)
		if err != nil {
			return nil, ErrHeldAmountService.Wrap(err)
		}

		payStubs = append(payStubs, payStub...)
	}

	return payStubs, nil
}

// GetPaymentCached retrieves payment data from particular satellite from storagenode database.
func (service *Service) GetPaymentCached(ctx context.Context, satelliteID storj.NodeID, period string) (_ *Payment, err error) {
	defer mon.Task()(&ctx, &satelliteID, &period)(&err)

	return service.db.GetPayment(ctx, satelliteID, period)
}

// dial dials the HeldAmount client for the satellite by id
func (service *Service) dial(ctx context.Context, satelliteID storj.NodeID) (_ *Client, err error) {
	defer mon.Task()(&ctx)(&err)

	address, err := service.trust.GetAddress(ctx, satelliteID)
	if err != nil {
		return nil, errs.New("unable to find satellite %s: %w", satelliteID, err)
	}

	conn, err := service.dialer.DialAddressID(ctx, address, satelliteID)
	if err != nil {
		return nil, errs.New("unable to connect to the satellite %s: %w", satelliteID, err)
	}

	return &Client{
		conn:                 conn,
		DRPCHeldAmountClient: pb.NewDRPCHeldAmountClient(conn.Raw()),
	}, nil
}

func stringToTime(period string) (_ time.Time, err error) {
	layout := "2006-01"
	per := period[0:7]
	result, err := time.Parse(layout, per)
	if err != nil {
		return time.Time{}, err
	}

	return result, nil
}

// TODO: improve it.
func parsePeriodRange(periodStart, periodEnd string) (periods []string, err error) {
	var yearStart, yearEnd, monthStart, monthEnd int

	start := strings.Split(periodStart, "-")
	if len(start) != 2 {
		return nil, ErrHeldAmountService.New("period start has wrong format")
	}
	end := strings.Split(periodEnd, "-")
	if len(start) != 2 {
		return nil, ErrHeldAmountService.New("period end has wrong format")
	}

	yearStart, err = strconv.Atoi(start[0])
	if err != nil {
		return nil, ErrHeldAmountService.New("period start has wrong format")
	}
	monthStart, err = strconv.Atoi(start[1])
	if err != nil || monthStart > 12 || monthStart < 1 {
		return nil, ErrHeldAmountService.New("period start has wrong format")
	}
	yearEnd, err = strconv.Atoi(end[0])
	if err != nil {
		return nil, ErrHeldAmountService.New("period end has wrong format")
	}
	monthEnd, err = strconv.Atoi(end[1])
	if err != nil || monthEnd > 12 || monthEnd < 1 {
		return nil, ErrHeldAmountService.New("period end has wrong format")
	}
	if yearEnd < yearStart {
		return nil, ErrHeldAmountService.New("period has wrong format")
	}
	if yearEnd == yearStart && monthEnd < monthStart {
		return nil, ErrHeldAmountService.New("period has wrong format")
	}

	for ; yearStart <= yearEnd; yearStart++ {
		lastMonth := 12
		if yearStart == yearEnd {
			lastMonth = monthEnd
		}
		for ; monthStart <= lastMonth; monthStart++ {
			format := "%d-%d"
			if monthStart < 10 {
				format = "%d-0%d"
			}
			periods = append(periods, fmt.Sprintf(format, yearStart, monthStart))
		}

		monthStart = 1
	}

	return periods, nil
}
