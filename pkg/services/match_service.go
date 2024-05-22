package services

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"d2api/config"
	"d2api/pkg/handlers"
	"d2api/pkg/models"
	"d2api/pkg/repository"
	"d2api/pkg/requests"
	"d2api/pkg/response"
	"d2api/pkg/scheduled_matches"
	"d2api/pkg/utils"

	"github.com/jasonodonnell/go-opendota"
	"github.com/paralin/go-dota2/protocol"
	"github.com/paralin/go-steam/steamid"
	"go.mongodb.org/mongo-driver/bson"
)

type MatchService struct {
	Handlers []*handlers.Handler
	Config   *config.Config
	Repo     *repository.Repository
}

func NewMatchService(handlers []*handlers.Handler, config *config.Config, repo *repository.Repository) MatchService {
	return MatchService{
		Handlers: handlers,
		Config:   config,
		Repo:     repo,
	}
}

func (s *MatchService) ScheduleMatch(req requests.CreateMatchReq) (string, error) {

	matchIdx := strconv.FormatInt(time.Now().UnixNano(), 10)
	utils.SetMatchRedis(matchIdx, models.MatchDetails{
		MatchStatus: models.MatchStatus{Status: "scheduled", MatchId: 0, IsTournamentMatch: false},
	})

	scheduled_matches.Add(matchIdx)

	go utils.MatchScheduleThread(&s.Handlers, req, matchIdx, s.Config.TimeToCancel)
	return matchIdx, nil
}

func (s *MatchService) GetMatch(matchIdx string) (interface{}, error) {
	match, err := utils.GetMatchRedis(matchIdx)
	if err != nil {
		return nil, err
	}

	handler, _, err := handlers.GetFirstHandler(s.Handlers)
	if err != nil {
		return nil, err
	}

	if match.Status == "cancelled" {
		return models.MatchCancel{MatchStatus: match.MatchStatus, Reason: match.CancelReason, TeamDidntShow: match.TeamDidntShow}, nil
	} else if match.Status == "scheduled" {
		lobby, err := utils.GetCurrentLobby(handler)
		if err != nil {
			log.Println("Failed to get lobby: ", err)
			return nil, err
		}
		return models.MatchLobby{MatchStatus: match.MatchStatus, Lobby: lobby}, nil
	}

	details, err := handler.DotaClient.RequestMatchDetails(context.Background(), match.MatchId)
	if err != nil {
		return nil, err
	}

	if *details.Result == 1 {
		match.MatchStatus.Status = "finished"
		err = utils.SetMatchRedis(matchIdx, *match)
		if err != nil {
			log.Println("Failed to set match:", err)
		}

		return models.MatchData{MatchStatus: match.MatchStatus, Match: details.Match}, nil
	} else if *details.Result == 2 {
		return match.MatchStatus, nil
	} else {
		return nil, errors.New("match not found")
	}
}

func (s *MatchService) GetMatchInfo(matchIdx string) (*response.MatchInfo, error) {
	data, err := s.GetMatch(matchIdx)
	if err != nil {
		return nil, err
	}

	var matchInfo *response.MatchInfo
	switch match := data.(type) {
	case models.MatchLobby:
		log.Println("MatchLobby")
		matchInfo = &response.MatchInfo{Status: match.MatchStatus.Status}
		for _, player := range match.Lobby.AllMembers {
			if *player.Team == *protocol.DOTA_GC_TEAM_DOTA_GC_TEAM_GOOD_GUYS.Enum() {
				matchInfo.RadiantPlayers = append(matchInfo.RadiantPlayers, *player.Id)
			} else if *player.Team == *protocol.DOTA_GC_TEAM_DOTA_GC_TEAM_BAD_GUYS.Enum() {
				matchInfo.DirePlayers = append(matchInfo.DirePlayers, *player.Id)
			}
		}
	case models.MatchData:
		log.Println("MatchData")
		matchInfo = &response.MatchInfo{Status: match.MatchStatus.Status}
	case models.MatchCancel:
		log.Println("MatchCancel")
		matchInfo = &response.MatchInfo{Status: match.MatchStatus.Status, Cancelled: true}
	case models.MatchDetails:
		log.Println("MatchDetails")
		matchInfo = &response.MatchInfo{Status: match.MatchStatus.Status}
	default:
		return nil, errors.New("unknown match type")
	}

	return matchInfo, nil
}

func (s *MatchService) GetPlayerHistoryOpenDota(steamId int64, limit int) (interface{}, error) {
	client := opendota.NewClient(http.DefaultClient)
	matches, _, err := client.PlayerService.Matches(steamId, &opendota.PlayerParam{Limit: limit})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

func (s *MatchService) GetPlayerHistory(steamId int64, limit int) (interface{}, error) {
	player := s.Repo.Get("players", bson.M{"_id": steamId})
	if player.Err() != nil {
		return nil, player.Err()
	}

	var playerModel models.Player
	if err := player.Decode(&playerModel); err != nil {
		return nil, err
	}

	var matchIds []uint64
	if len(playerModel.Matches) < limit {
		matchIds = playerModel.Matches
	} else {
		matchIds = playerModel.Matches[:limit]
	}

	handler, _, err := handlers.GetFirstHandler(s.Handlers)
	if err != nil {
		return nil, err
	}

	var matches []*protocol.CMsgGCMatchDetailsResponse
	for _, matchId := range matchIds {
		details, err := handler.DotaClient.RequestMatchDetails(context.Background(), matchId)
		if err != nil {
			return nil, err
		}

		matches = append(matches, details)
	}

	return matches, nil
}

func (s *MatchService) ReinvitePlayers(req requests.ReinvitePlayersReq) error {
	match, err := utils.GetMatchRedis(strconv.Itoa(req.MatchIdx))
	if err != nil {
		return err
	}

	handler := s.Handlers[match.HandlerId]
	for _, player := range req.Players {
		handler.DotaClient.InviteLobbyMember(steamid.SteamId(player))
	}

	return nil
}
