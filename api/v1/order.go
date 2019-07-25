package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo"
	. "github.com/oldfritter/goDCE/models"
	"github.com/oldfritter/goDCE/utils"
	"github.com/shopspring/decimal"
	"github.com/streadway/amqp"
)

func V1GetOrder(context echo.Context) error {
	user := context.Get("current_user").(User)

	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	var order Order
	if mainDB.Where("id = ? AND user_id = ?", context.QueryParam("id"), user.Id).
		First(&order).RecordNotFound() {
		return utils.BuildError("1020")
	}
	return context.JSON(http.StatusOK, order)
}

func V1GetOrders(context echo.Context) error {
	user := context.Get("current_user").(User)

	var market Market
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	if mainDB.Where("name = ?", context.QueryParam("market")).First(&market).RecordNotFound() {
		return utils.BuildError("1021")
	}
	state := 100
	if context.QueryParam("state") != "" {
		if context.QueryParam("state") == "done" {
			state = 200
		} else if context.QueryParam("state") == "cancel" {
			state = 0
		}
	}
	limit := 30
	if context.QueryParam("limit") != "" {
		limit, _ = strconv.Atoi(context.QueryParam("limit"))
		if limit > 2000 {
			limit = 30
		}
	}
	page := 1
	if context.QueryParam("page") != "" {
		page, _ = strconv.Atoi(context.QueryParam("page"))
		if page < 1 {
			page = 1
		}
	}
	day := ""
	if context.QueryParam("day") != "" {
		day = context.QueryParam("day")
	}
	orderParam := "id DESC"
	if context.QueryParam("order_by") == "asc" {
		orderParam = "id ASC"
	}
	var orders []Order
	if day == "" {
		if mainDB.Order(orderParam).
			Where("market_id = ? AND user_id = ? AND state = ? ", market.Id, user.Id, state).
			Offset(limit * (page - 1)).Limit(limit).Find(&orders).RecordNotFound() {
			return utils.BuildError("1020")
		}
	} else {
		if mainDB.Order(orderParam).
			Where("market_id = ? AND user_id = ? AND state = ? AND date(created_at) = ?", market.Id, user.Id, state, day).
			Offset(limit * (page - 1)).Limit(limit).Find(&orders).RecordNotFound() {
			return utils.BuildError("1020")
		}
	}
	return context.JSON(http.StatusOK, orders)
}

func V1PostOrders(context echo.Context) error {
	params := context.Get("params").(map[string]string)
	if params["price"] == "" {
		return utils.BuildError("1024")
	}
	if params["volume"] == "" {
		return utils.BuildError("1023")
	}
	user := context.Get("current_user").(User)
	var market Market
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	if mainDB.Where("name = ?", params["market"]).First(&market).RecordNotFound() {
		return utils.BuildError("1021")
	}
	side := context.QueryParam("side")
	price, _ := decimal.NewFromString(params["price"])
	volume, _ := decimal.NewFromString(params["volume"])
	price, volume = price.Truncate(int32(market.BidFixed)), volume.Truncate(int32(market.AskFixed))
	if price.LessThanOrEqual(decimal.Zero) {
		return utils.BuildError("1024")
	}
	if volume.LessThanOrEqual(decimal.Zero) {
		return utils.BuildError("1023")
	}
	var orderType string
	locked := volume
	if side == "buy" {
		locked = volume.Mul(price)
		orderType = "OrderBid"
	} else if side == "sell" {
		orderType = "OrderAsk"
	} else {
		return utils.BuildError("1022")
	}
	order := Order{
		Source:       "APIv1",
		State:        100,
		UserId:       user.Id,
		MarketId:     market.Id,
		Volume:       volume,
		OriginVolume: volume,
		Price:        price,
		OrderType:    "limit",
		Type:         orderType,
		Locked:       locked,
		OriginLocked: locked,
	}
	err := tryToChangeAccount(context, &order, &market, side, user.Id, 9)
	if err == nil {
		pushMessageToMatching(&order, &market, "submit")
		response := utils.SuccessResponse
		response.Body = order
		return context.JSON(http.StatusOK, response)
	} else {
		return err
	}
}

type OrderAttr struct {
	Side             string          `json:"side"`
	NewClientOrderId string          `json:"new_client_order_id"`
	OrderType        string          `json:"order_type"`
	Price            decimal.Decimal `json:"price"`
	Volume           decimal.Decimal `json:"volume"`
}

func V1PostOrdersMulti(context echo.Context) error {
	params := context.Get("params").(map[string]string)
	user := context.Get("current_user").(User)
	var market Market
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	if mainDB.Where("name = ?", params["market"]).First(&market).RecordNotFound() {
		return utils.BuildError("1021")
	}
	var orderAttrs []OrderAttr
	json.Unmarshal([]byte(strings.Replace(params["orders"], `\"`, `"`, -1)), &orderAttrs)
	orders := make([]interface{}, len(orderAttrs))
	for i, orderAttr := range orderAttrs {
		var orderType string
		locked := orderAttr.Volume
		price, volume := orderAttr.Price.Truncate(int32(market.BidFixed)), orderAttr.Volume.Truncate(int32(market.AskFixed))
		if price.LessThanOrEqual(decimal.Zero) {
			return utils.BuildError("1024")
		}
		if volume.LessThanOrEqual(decimal.Zero) {
			return utils.BuildError("1023")
		}
		if orderAttr.Side == "buy" {
			locked = orderAttr.Volume.Mul(orderAttr.Price)
			orderType = "OrderBid"
		} else if orderAttr.Side == "sell" {
			orderType = "OrderAsk"
		} else {
			return utils.BuildError("1022")
		}
		order := Order{
			Source:       context.Param("platform") + "-APIv1",
			State:        100,
			UserId:       user.Id,
			MarketId:     market.Id,
			Volume:       volume,
			OriginVolume: volume,
			Price:        price,
			OrderType:    "limit",
			Type:         orderType,
			Locked:       locked,
			OriginLocked: locked,
		}
		err := tryToChangeAccount(context, &order, &market, orderAttr.Side, user.Id, 9)
		if err == nil {
			pushMessageToMatching(&order, &market, "submit")
			orders[i] = order
		} else {
			orders[i] = nil
		}

	}
	return context.JSON(http.StatusOK, orders)
}

func V1PostOrderDelete(context echo.Context) error {
	params := context.Get("params").(map[string]string)
	user := context.Get("current_user").(User)
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	var order Order
	if mainDB.Where("state = 100 AND id = ? AND user_id = ?", params["id"], user.Id).First(&order).RecordNotFound() {
		return utils.BuildError("2004")
	}
	pushMessageToMatching(&order, &order.Market, "cancel")
	return context.JSON(http.StatusOK, order)
}

func V1PostOrdersDelete(context echo.Context) error {
	params := context.Get("params").(map[string]string)
	user := context.Get("current_user").(User)
	ids := strings.Split(params["ids"], ",")
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	var orders []Order
	if mainDB.Where("id in (?) AND user_id = ?", ids, user.Id).Find(&orders).RecordNotFound() {
		return utils.BuildError("2004")
	}
	for _, order := range orders {
		if order.State == 100 {
			pushMessageToMatching(&order, &order.Market, "cancel")
		}
	}
	return context.JSON(http.StatusOK, orders)
}

func V1PostOrdersClear(context echo.Context) error {
	params := context.Get("params").(map[string]string)
	user := context.Get("current_user").(User)
	var market Market
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	if mainDB.Where("name = ?", params["market"]).First(&market).RecordNotFound() {
		return utils.BuildError("1021")
	}
	var orders []Order
	if market.Id == 0 {
		mainDB.Where("state = 100 AND user_id = ?", user.Id).Find(&orders)
		for _, order := range orders {
			pushMessageToMatching(&order, &market, "cancel")
		}
	} else {
		mainDB.Where("state = 100 AND user_id = ? AND market_id = ?", user.Id, market.Id).Find(&orders)
		for _, order := range orders {
			pushMessageToMatching(&order, &market, "cancel")
		}
	}
	return context.JSON(http.StatusOK, orders)
}

func pushMessageToMatching(order *Order, market *Market, option string) {
	payload := MatchingPayload{
		Action: option,
		Order: OrderJson{
			Id:        (*order).Id,
			MarketId:  (*order).MarketId,
			Type:      (*order).OType(),
			OrderType: (*order).OrderType,
			Volume:    (*order).Volume,
			Price:     (*order).Price,
			Locked:    (*order).Locked,
			Timestamp: (*order).CreatedAt.Unix(),
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		fmt.Println("error:", err)
	}

	err = utils.PublishMessageWithRouteKey(
		utils.AmqpGlobalConfig.Exchange.Matching["key"],
		market.Code, "text/plain",
		&b,
		amqp.Table{},
		amqp.Persistent,
	)
	if err != nil {
		fmt.Println("{ error:", err, "}")
		panic(err)
	}
}

func tryToChangeAccount(context echo.Context, order *Order, market *Market, side string, user_id, times int) error {
	mainDB := utils.MainDbBegin()
	defer mainDB.DbRollback()
	var account Account
	if side == "buy" {
		if mainDB.Where("user_id = ?", user_id).Where("currency = ?", (*market).BidCurrencyId).
			First(&account).RecordNotFound() {
			return utils.BuildError("1026")
		}
	} else if side == "sell" {
		if mainDB.Where("user_id = ?", user_id).Where("currency = ?", (*market).AskCurrencyId).
			First(&account).RecordNotFound() {
			return utils.BuildError("1026")
		}
	}
	if account.Balance.Sub((*order).Locked).IsNegative() {
		return utils.BuildError("1041")
	}
	result := mainDB.Create(order)
	if result.Error != nil {
		mainDB.DbRollback()
		if times > 0 {
			(*order).Id = 0
			return tryToChangeAccount(context, order, market, side, user_id, times-1)
		}
	}
	err := account.LockFunds(mainDB, (*order).Locked, ORDER_SUBMIT, (*order).Id, "Order")
	if err == nil {
		mainDB.DbCommit()
		return nil
	}

	mainDB.DbRollback()
	if times > 0 {
		(*order).Id = 0
		time.Sleep(500 * time.Millisecond)
		return tryToChangeAccount(context, order, market, side, user_id, times-1)
	}
	return utils.BuildError("2002")
}
