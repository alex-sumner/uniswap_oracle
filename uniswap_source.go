package sources

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	bf "github.com/ALTree/bigfloat"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
	"github.com/strips-finance/rabbit-dex-backend/pricing/uniswap_v3_pool"
)

type UniswapSource struct {
	poolAddress       string
	tokenAddress      string
	providerUrl       string
	averagingInterval uint32
	ethClient         *ethclient.Client
	poolAbi           abi.ABI
	poolInstance      *uniswap_v3_pool.UniswapV3Pool
	updateInterval    time.Duration
	token0IsBase      bool
	priceDecimalsMult *big.Float
	lastUpdated       time.Time
	lastPrice         *big.Float
}

func stripPrefix(input string, charsToRemove int) string {
	asRunes := []rune(input)
	return string(asRunes[charsToRemove:])
}

func NewUniswapSource(providerUrl string, averagingInterval uint32, tokenAddress string, priceDecimals int) (*UniswapSource, error) {

	us := &UniswapSource{
		providerUrl:       providerUrl,
		updateInterval:    time.Second,
		averagingInterval: averagingInterval,
		tokenAddress:      tokenAddress,
	}

	ten := new(big.Float).SetInt64(10)
	decMult := new(big.Float).SetInt64(1)
	for i := 0; i < priceDecimals; i++ {
		decMult.Mul(decMult, ten)
	}
	us.priceDecimalsMult = decMult
	contractAbi, err := abi.JSON(strings.NewReader(
		string(uniswap_v3_pool.UniswapV3PoolMetaData.ABI)))
	if err != nil {
		return nil,
			fmt.Errorf("Error retrieving UniswapV3Pool contract ABI: %s", err.Error())
	}
	us.poolAbi = contractAbi

	logrus.Infof("created Uniswap source, provider url: %s", us.providerUrl)

	return us, nil
}

func (us *UniswapSource) Dial(ctx context.Context, poolAddress string) (err error) {
	if strings.HasPrefix(poolAddress, "0x") {
		poolAddress = stripPrefix(poolAddress, 2)
	}
	if strings.HasPrefix(us.tokenAddress, "0x") {
		us.tokenAddress = stripPrefix(us.tokenAddress, 2)
	}
	us.poolAddress = poolAddress
	var client *ethclient.Client
	var instance *uniswap_v3_pool.UniswapV3Pool
	var sleepFor = 4 * time.Second
	for i := 0; i < 5; i++ {
		client, err = ethclient.DialContext(ctx, us.providerUrl)
		if err == nil {
			poolAddr := common.HexToAddress(us.poolAddress)
			instance, err = uniswap_v3_pool.NewUniswapV3Pool(poolAddr, client)
			if err == nil {
				break
			} else {
				err = fmt.Errorf("Error creating Uniswap pool contract instance: %s", err.Error())
			}
		}
		time.Sleep(sleepFor)
		sleepFor = sleepFor * 2
	}
	if err != nil {
		return fmt.Errorf("Error dialing eth client: %s", err.Error())
	}
	tokenAddr := common.HexToAddress(us.tokenAddress)
	token0, err := instance.Token0(nil)
	if err != nil {
		return fmt.Errorf("Error reading uniswap token0 info: %s", err.Error())
	}
	if token0 == tokenAddr {
		us.token0IsBase = true
	} else {
		token1, err := instance.Token1(nil)
		if err != nil {
			return fmt.Errorf("Error reading uniswap token1 info: %s", err.Error())
		}
		if token1 == tokenAddr {
			us.token0IsBase = false
		} else {
			return fmt.Errorf("Error token %s not found in pool %s", us.tokenAddress, poolAddress)
		}
	}
	if err != nil {
		return fmt.Errorf("Error reading uniswap slot1 info: %s", err.Error())
	}
	logrus.Info("successfully connected to eth client")
	us.ethClient = client
	us.poolInstance = instance
	return nil
}

func (us *UniswapSource) Connection() net.Conn {
	return nil
}

func (us *UniswapSource) HangUp() error {
	if us.ethClient != nil {
		us.ethClient.Close()
	}
	return nil
}

func (us *UniswapSource) getPrice() (*big.Float, error) {
	slot0, err := us.poolInstance.Slot0(nil)
	if err != nil {
		return nil, fmt.Errorf("Error reading uniswap slot0 info: %s", err.Error())
	}
	fmt.Printf("SqrtPriceX96: %s\n", slot0.SqrtPriceX96.String())
	fmt.Printf("Tick: %d\n", slot0.Tick)
	fmt.Printf("ObservationIndex: %d\n", slot0.ObservationIndex)
	fmt.Printf("ObservationCardinality: %d\n", slot0.ObservationCardinality)
	fmt.Printf("ObservationCardinalityNext: %d\n", slot0.ObservationCardinalityNext)
	fmt.Printf("FeeProtocol: %d\n", slot0.FeeProtocol)
	fmt.Printf("Unlocked: %v\n", slot0.Unlocked)

	//simulate a web socket connection by sleeping if we've updated recently
	if time.Since(us.lastUpdated) < us.updateInterval {
		time.Sleep(us.updateInterval - time.Since(us.lastUpdated))
	}
	var secondsAgo = []uint32{us.averagingInterval, 0}
	logrus.Infof("secondsAgo %d %d", secondsAgo[0], secondsAgo[1])
	observed, err := us.poolInstance.Observe(nil, secondsAgo)
	if err != nil {
		return nil, err
	}
	tickCumulatives := observed.TickCumulatives
	logrus.Infof("tickCumulatives [0] %d [1] %d", tickCumulatives[0], tickCumulatives[1])
	// SecondsPerLiquidityCumulativeX128s := observed.SecondsPerLiquidityCumulativeX128s
	var ticks = big.NewFloat(0).SetInt(big.NewInt(0).Sub(tickCumulatives[1], tickCumulatives[0]))
	logrus.Infof("ticks  %d", ticks)
	var meanTick = big.NewFloat(0).Quo(ticks, big.NewFloat(0).SetInt64(int64(us.averagingInterval)))
	logrus.Infof("meanTick  %d", meanTick)
	sqrtRatio := getSqrtRatioAtTick(meanTick)
	logrus.Infof("sqrtRatio  %d", sqrtRatio)
	ratio := new(big.Float).Mul(sqrtRatio, sqrtRatio)
	logrus.Infof("ratio  %d", ratio)
	var numerator, divisor *big.Float
	if us.token0IsBase {
		numerator = ratio
		divisor = new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil))
	} else {
		numerator = new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil))
		divisor = ratio
	}
	logrus.Infof("Quo %d %d", numerator, divisor)
	price := new(big.Float).Mul(new(big.Float).Quo(numerator, divisor), us.priceDecimalsMult)
	logrus.Infof("price  %d", price)
	us.lastPrice = price
	us.lastUpdated = time.Now()
	return price, nil
}

func (us *UniswapSource) ReadServerData() (response []byte, err error) {
	price, err := us.getPrice()
	if err != nil {
		return nil, err
	}
	return price.GobEncode()
}

func getSqrtRatioAtTick(tick *big.Float) *big.Float {
	onePoint0001 := new(big.Float).SetFloat64(1.0001)
	twoPower96 := new(big.Float).SetFloat64(1 << 96)
	powResult := bf.Pow(onePoint0001, tick)
	sqrtResult := bf.Sqrt(powResult)
	return new(big.Float).Mul(sqrtResult, twoPower96)
}
