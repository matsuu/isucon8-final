package bench

import "time"

const (
	// Timeouts
	ClientTimeout = 10 * time.Second
	InitTimeout   = 10 * time.Second

	// tradeをpollingする間隔
	TradePollingInterval = 1 * time.Second

	// Scores
	SignupScore         = 1
	SigninScore         = 1
	GetTradesScore      = 1
	PostBuyOrdersScore  = 5
	PostSellOrdersScore = 5
	GetBuyOrdersScore   = 1
	GetSellOrdersScore  = 1
	TradeSuccessScore   = 10

	// error
	AllowErrorMin = 10 // levelによらずここまでは許容範囲というエラー数
	AllowErrorMax = 50 // levelによらずこれ以上は許さないというエラー数
)
