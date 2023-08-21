package providers

import (
	"math/big"
	"time"
)

func NewUniswapProvider(poolAddress string, source WebSocketSource, maxDelay time.Duration, multiplier float64) *PriceProvider {
	provider := NewPriceProvider(poolAddress, "", source, maxDelay)
	provider.extractPrice = func(priceResponse []byte, maxDelay time.Duration) (price float64, err error) {
		bprice := new(big.Float)
		err = bprice.GobDecode(priceResponse)
		if err != nil {
			return 0, err
		}
		price, _ = bprice.Mul(bprice, big.NewFloat(multiplier)).Float64()
		return price, nil
	}
	return provider
}
