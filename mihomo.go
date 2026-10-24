package main

import (
	"bytes"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	proxyProviderInterval = 3600
	directRulesInterval   = 3600
)

// Sourced from the 463-package list published in:
// https://github.com/net4people/bbs/issues/605#issuecomment-4232057105
const directAndroidPackageList = `
ru.aliexpress.buyer
com.logistic.sdek
ru.cdek.courier
ru.cdek.ek5
ru.plazius.cofix
com.lamoda.lite
ru.more.play
tv.okko.androidtv
com.BOGG.ART.VKT
com.allgoritm.youla
com.my.mygamesapp
com.uma.musicvk
com.vk.admin
com.vk.calls
com.vk.clips
com.vk.im
com.vk.love
com.vk.mail
com.vk.tv
com.vk.vkvideo
com.vkontakte.android
games.my.cloud
games.my.cloud.tv
live.vkplay.app
ru.mail.biz.avocado
ru.mail.cloud
ru.mail.horo.android
ru.mail.intranet
ru.mail.mailapp
ru.mail.my
ru.mail.search.electroscope
ru.ok.android
ru.ok.dating
ru.vk.hrtek
ru.zen.android
com.x5.courierapp
com.x5.pickerapp.ff
apps.yclients1
com.yclients.yplaces
ru.alfabank.mobile.android
ru.alfabank.oavdo.amc
ru.alfadirect.app
ru.lifepay.pos.alfapos
ru.m4bank.softpos.alfaru
com.vkusvill.courier
com.vkusvill.vacancy
ru.vkusvill
app.subscab.android
com.kaspersky.kes
com.kaspersky.passwordmanager
com.kaspersky.petka
com.kaspersky.safekids
com.kaspersky.secure.connection
com.kaspersky.who_calls
com.kms.free
ru.ntv.client
ru.ntv.client.tv
ru.ntv.today
ru.nspk.mir.loyalty
ru.nspk.mirpay
ru.nspk.sbpay
ru.intech.lki
ru.m4bank.softpos.rshb
ru.rshb.dbo
ru.rshb.dboul
ru.rshb.digitalCoworkerOffice
ru.rshb.ips_qr
ru.rshb.privatebusiness
ru.rshb.svoe_rodnoe
ru.rshb.svoedom_android
ru.rshb.svojfermer
ruagro.start.tech
com.idamob.tinkoff.android
com.journal_tj
com.uchebnik_tj
ru.tbank.myt
ru.tinkoff.bnpl
ru.tinkoff.investing
ru.tinkoff.lifestyle.fuel.sme
ru.tinkoff.mvno
ru.tinkoff.posterminal
ru.tinkoff.sme
az.vtb.android
com.horosho.tips
ru.lifepay.pos.vtb
ru.m4bank.softpos.vtbpos
ru.vtb.corporate.android.main_application
ru.vtb.invest
ru.vtb.smb
ru.vtb24.mobilebanking.android
ru.gazprombank.android.mobilebank.app
ru.gazprombank.broker
ru.gazprombank.mobile.gpbbusiness
com.dexp01.reoka01.com
com.dnsriteil.delivery
com.dnsriteil.dns_authenticator
com.info.smartphone.a1cmobilecomponent
ru.dns.shop.android
ru.megapolis.rms.servicedesk
ru.mos.helper
ru.mosgorpass
ru.mosmetro.metro
ru.mosparking.appnew
com.programmisty.emiasapp
ru.mgfoms.pandora
tech.clink.emias.emiasqrcode
com.discover.moscow.lite
com.moscowsport
com.tdm.messenger
org.ts.mapp
ru.altarix.mos.pgu
ru.dit.smartstaff
ru.mos.MskLongevity
ru.mos.aiso.m
ru.mos.amina.app
ru.mos.analytics.app.prod
ru.mos.app
ru.mos.cms.erzapp
ru.mos.cms.lkyo
ru.mos.dcloud.client
ru.mos.ed
ru.mos.gz
ru.mos.myid
ru.mos.ossigbuilder
ru.mos.ourcity
ru.mos.pet
ru.mos.polls
ru.mos.um
ru.netris.videocity
ru.open.gisokmp
ru.leroymerlin.employee
ru.leroymerlin.mobile
ru.oneme.app
com.equeo.megafondraiv
com.mcptt
com.megafon.kk
com.megafon.maps
com.megafon.mcca.android.store
com.megafon.rk
megafon.transport.mobile
ru.megafon.mlk
ru.megafon.vn.client
ru.megalabs.multifon
ru.netbynet.app.personal_cabinet
ru.systtech.megafon.mobile
com.pobeda.anniversary
ru.fanid
ru.gosuslugi.auto
ru.gosuslugi.culture
ru.gosuslugi.goskey
ru.gosuslugi.migrant
ru.gosuslugi.pos
ru.gosuslugi.pos.executor
ru.gosuslugi.school
ru.gosuslugi.volunteer
ru.imteleport.plp2
ru.okbmei.gosservice
ru.rostel
team.rtds.checkcontrol
com.gsgroup.secondscreen
com.gsgroup.smarthome
com.gsgroup.tricoloronline
com.gsgroup.tricoloronline.mobile
com.zebrains.trkid
ru.iflex.android.a3colortv
tv.tricolor.video
com.rzd.eventsrzd
ru.rzd.cargolk
ru.rzd.newsdo.skills
ru.rzd.pass
com.wbextdelivery
com.wbpoint.android.prod.release
com.wbstream_app
com.wildberries.consultations
com.wildberries.ru
com.wildberries.wbkey
ru.wb.courier
ru.wb.go
ru.wb.guru
ru.wb.wband
ru.wildberries.books
ru.wildberries.manager
ru.wildberries.team
ru.wildberries.team2
ru.wildberries.wbpayclient
wb.partners
wildberries.business
com.indygomobi.detifm
com.indygomobi.relaxfm
com.indygomobi.zenit
ru.ideast.humor
ru.ideast.nrj
ru.ideast.romantika
ru.ideast.ru101.rustore
ru.profmedia.avtoradio
ru.profmedia.likefm
ru.vkpm.comedy
com.drweb
com.drweb.familysecure
otello.dgis.ru
ru.dublgis.dgismobile
ru.dublgis.dgismobile4preview
com.asmo.infometeos
com.gmeteos.infometeos
com.pogoda.infometeos
ru.ivi.client
ru.ozon.app.android
ru.ozon.chatzone
ru.ozon.fintech.apvz
ru.ozon.fintech.finance
ru.ozon.fintech.sme
ru.ozon.flex
ru.ozon.fresh
ru.ozon.hire
ru.ozon.magistral
ru.ozon.maple_app
ru.ozon.ozon_pvz
ru.ozon.profit
ru.ozon.select
ru.ozon.seller_app
ru.ozon.tv
travel.ozon.mobile
com.avito.android
com.icemobile.lenta.prod
com.lenta.employee
com.lenta.lentochka_service
ru.lenta.lentochka
ru.mntk.dostavka.prod
ru.utk.dostavka
ru.litres.android
ru.litres.android.readfree
ru.litres.androidtv
ru.livelib.client
ru.mybook
com.ncloudtech.cloudoffice
com.ncloudtech.cloudoffice.b2b
ru.ncloudtech.mec
ru.ncloudtech.squadus
com.grotem.match
com.match.club
com.matchtv.rustore
business.gid.hero
business.gid.universe
ru.gazprompay.android
ru.gid.app
ru.gid.eventsapp
ru.gid.most
gpm.tnt_premier
one.premier.rustoretv
ru.ritmmedia.yappy
ru.rutube.RutubeKids
ru.rutube.app
ru.rutube.app.tv
ru.rutube.studio
ru.yota.android
chat.strelka
ru.sbcs.store
com.flowwow
ru.tntmusic.tv
ru.hh.android
ru.hh.employer.android
ru.setka
com.chaevieprosto.app
com.deliveryclub
com.edadeal.android
com.reserveonline.tablet.sr.suffix
com.routeq.app
com.yandex.aliceapp
com.yandex.bank
com.yandex.bluecollars
com.yandex.browser
com.yandex.browser.corp
com.yandex.browser.lite
com.yandex.browser.tv
com.yandex.calendar.app
com.yandex.corp.tracking.app
com.yandex.internetometer
com.yandex.iot
com.yandex.lavka
com.yandex.mobile.drive
com.yandex.mobile.realty
com.yandex.rhythm
com.yandex.searchapp
com.yandex.shedevrus
com.yandex.tasks.androidapp
com.yandex.yamb
com.zebrainy.skazbuka
home.pragma.com
home.tuvio.com
jamhome.ru.lktsj
ru.beru.android
ru.foodfox.client
ru.foodfox.vendor
ru.kinopoisk
ru.kinopoisk.tv
ru.mercaux.app
ru.plus.bookmate
ru.yandex.androidkeyboard
ru.yandex.cloud
ru.yandex.cloud.tracker
ru.yandex.direct
ru.yandex.disk
ru.yandex.drawie
ru.yandex.games
ru.yandex.games.tv
ru.yandex.key
ru.yandex.mail
ru.yandex.market.partner
ru.yandex.market.pickuppointpro
ru.yandex.metro
ru.yandex.mobile.afisha
ru.yandex.mobile.avia
ru.yandex.mobile.fmradio
ru.yandex.mobile.gasstations
ru.yandex.music
ru.yandex.practicum.flutter_practicum
ru.yandex.rasp
ru.yandex.readie
ru.yandex.rovertoy.app
ru.yandex.searchplugin
ru.yandex.smartreserve.aon
ru.yandex.subtitles
ru.yandex.taxi
ru.yandex.taximeter
ru.yandex.telemost
ru.yandex.translate
ru.yandex.travel
ru.yandex.travel.extranet
ru.yandex.uber
ru.yandex.weatherplugin
ru.yandex.yandexmaps
ru.yandex.yandexnavi
ru.yandex_team.calendar_app
ru.eapteka.courier
ru.getpharma.eapteka
ru.cian.main
com.notissimus.allinstruments.android
club.chizhik
ru.pyaterochka.app.browser
ru.x5.foodru.rs
ru.x5.key
ru.x5.mfpnew
ru.x5.mrm_android.personalDevice
ru.x5.omni
ru.x5.opermax.aura
ru.x5.rooms
ru.x5.wl.slata
ru.x5.wrs2
ru.x5.x5job
com.mvideo.mvseller
ru.filit.mvideo.b2c
ru.mvideo.service
ru.mvm.eldo
ru.newamelia.mvideo
ru.t2.wfmapp.prod
ru.tele2.app.wtt
ru.tele2.appseller
ru.tele2.cyberhero.pro
ru.tele2.mytele2
ru.tele2.nasvyzi
ru.tele2.sms.blocker
tele2.vats
ru.tutu.etrains
ru.tutu.tutu_emp
ru.ntvplus.service
tv.ntvplus.app
tv.ntvplus.app.tv
ru.sigma.gisgkh
ru.aeroflot
logo.com.mbanking
ru.psbank.invest
ru.psbank.msb.dev.psb_appstore
sodruzhestvo.android.app
com.iwedia.ui.beeline.atv
com.spbtv.rosing
intech.global.com.beeline
ru.beeline.beebookreader
ru.beeline.cloud
ru.beeline.dol
ru.beeline.messengerx
ru.beeline.mwpsapp
ru.beeline.pro
ru.beeline.services
ru.beeline.tve.android
com.kazanexpress.ke_app
com.magnit.delivery.courier
com.wms_mm_point.mm_delivery_point
ru.compass.tag
ru.magnit.hrtest.android
ru.magnit.mesmag
ru.magnit.tag
ru.magnit.tms.armhde
ru.magnit.vendorapp
ru.tander.magnit
ru.tander.mobile_scout_flutter
com.birzha_rt
com.dartit.RTcabinet
com.dartit.mobileagent
com.rostelecom.printservice
org.rtk.cleaning
ru.onlime.my.app
ru.rostelecom.ideas
ru.rostelecom.vatsmobilelk
ru.rt.key
ru.rt.life
ru.rt.masterkey
ru.rt.mb_installer
ru.rt.mobileoffice
ru.rt.ritm
ru.rt.smarthome
ru.rt.video.app.mobile.b2b
ru.rt.videocomfort
ru.rt.wfm
ru.rtdc.kakdela
ru.yozhka
com.goodok.mts
com.mts.who_calls
me.membrana.kids.parent.app
ru.mts.books.droid
ru.mts.live.app
ru.mts.mtscode
ru.mts.mtstv
ru.mts.music.android
ru.mts.mymts
ru.mts.pay
ru.mts.twomem
ru.mts.vdome.resident
com.navplatform.navigator.public
com.sberbank.sberpravo
com.sdkit.search.app
ru.sber.csradar
ru.sberbank.investor
ru.sberbank.onlineencashment
ru.sberbank.sberkids
ru.sberbank.sbersign
ru.sberbank_sbbol
ru.sberbankmobile
psb.market.app
ru.perekrestok.app
ru.pikabu.android
com.octopod.russianpost.client.android
ru.russianpost.pechkin
com.mcd.superapp
ru.ideast.championat
ru.rambler.horoscopes.twa
ru.rambler.mail
ru.rt.video.app.mobile
ru.rt.video.app.tv
ru.wink.music
ru.wink.music.tv
ru.fdoctor.fdocmob
ru.crpt.localmodule
ru.crptech.b2bmark
ru.crptech.foiv
ru.crptech.mark
ru.crptech.pharma_scanner
com.b2bdomru.domrubusiness
com.ertelecom.agent
com.ertelecom.domrutv
com.ertelecom.domrutvstb
com.ertelecom.smarthome
com.ertelecom.vats
ru.cctvb2b.dom.app
`

var directAndroidPackages = strings.Fields(directAndroidPackageList)

type mihomoProxy struct {
	Name              string
	Server            string
	Port              int
	UUID              string
	Flow              string
	ServerName        string
	ClientFingerprint string
	PublicKey         string
	ShortID           string
}

func buildMihomoConfig(baseURL, token string) []byte {
	providerURL := strings.TrimRight(baseURL, "/") + "/subscribe/" + token + "/proxies"
	rulesURL := strings.TrimRight(baseURL, "/") + "/mihomo/direct-rules"
	providerPath := "./proxy_providers/vps-reality-" + token + ".yaml"
	var output bytes.Buffer
	fmt.Fprintf(&output, `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: true
find-process-mode: always

proxy-providers:
  vps-reality:
    type: http
    url: %s
    path: %s
    interval: %d
    proxy: DIRECT
    health-check:
      enable: true
      url: "https://www.gstatic.com/generate_204"
      interval: 300
      timeout: 5000
      lazy: true

rule-providers:
  direct-rules:
    type: http
    behavior: classical
    format: yaml
    url: %s
    path: ./rule_providers/vps-reality-direct.yaml
    interval: %d
    proxy: DIRECT

proxy-groups:
  - name: VPN
    type: select
    default-selected: "Рекомендуемый"
    proxies:
      - "Рекомендуемый"
    use:
      - vps-reality
  - name: "Рекомендуемый"
    type: fallback
    hidden: true
    use:
      - vps-reality
    url: "https://www.gstatic.com/generate_204"
    interval: 300
    lazy: true
    empty-fallback: DIRECT

rules:
  - DOMAIN-SUFFIX,localhost,DIRECT
  - DOMAIN-SUFFIX,local,DIRECT
  - DOMAIN-SUFFIX,lan,DIRECT
  - DOMAIN-SUFFIX,home.arpa,DIRECT
  - IP-CIDR,10.0.0.0/8,DIRECT
  - IP-CIDR,172.16.0.0/12,DIRECT
  - IP-CIDR,192.168.0.0/16,DIRECT
  - IP-CIDR,100.64.0.0/10,DIRECT
  - IP-CIDR,127.0.0.0/8,DIRECT
  - IP-CIDR,169.254.0.0/16,DIRECT
  - IP-CIDR6,::1/128,DIRECT
  - IP-CIDR6,fc00::/7,DIRECT
  - IP-CIDR6,fe80::/10,DIRECT
  - RULE-SET,direct-rules,DIRECT
  - MATCH,VPN
`, yamlQuote(providerURL), yamlQuote(providerPath), proxyProviderInterval, yamlQuote(rulesURL), directRulesInterval)
	return output.Bytes()
}

func buildMihomoProvider(state State, user *User) []byte {
	var output, recommended bytes.Buffer
	preferredID := user.RecommendedServerID
	usedNames := make(map[string]int)
	for _, server := range state.Servers {
		if !userAllowsServer(*user, server) {
			continue
		}
		link, exists := user.Links[server.ID]
		if !exists || link.Status != "ready" || link.URI == "" {
			continue
		}
		name := serverDisplayName(server)
		usedNames[name]++
		if usedNames[name] > 1 {
			name = fmt.Sprintf("%s (%d)", name, usedNames[name])
		}
		proxy, err := parseVLESSReality(link.URI, name)
		if err != nil {
			continue
		}
		// Assign names in the original order so changing priority does not rename nodes.
		if server.ID == preferredID {
			writeMihomoProxy(&recommended, proxy)
		} else {
			writeMihomoProxy(&output, proxy)
		}
	}
	if recommended.Len() == 0 && output.Len() == 0 {
		return []byte("proxies: []\n")
	}
	return []byte("proxies:\n" + recommended.String() + output.String())
}

func buildDirectRules() []byte {
	var output bytes.Buffer
	output.WriteString("payload:\n")
	fmt.Fprintf(&output, "  - %s\n", yamlQuote("DOMAIN-SUFFIX,ru"))
	fmt.Fprintf(&output, "  - %s\n", yamlQuote("DOMAIN-SUFFIX,2ip.io"))
	for _, packageName := range directAndroidPackages {
		fmt.Fprintf(&output, "  - %s\n", yamlQuote("PROCESS-NAME,"+packageName))
	}
	return output.Bytes()
}

func parseVLESSReality(raw, name string) (mihomoProxy, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "vless" || parsed.User == nil {
		return mihomoProxy{}, fmt.Errorf("invalid VLESS URI")
	}
	uuid := parsed.User.Username()
	host := parsed.Hostname()
	if uuid == "" || host == "" {
		return mihomoProxy{}, fmt.Errorf("VLESS URI is incomplete")
	}
	port := 443
	if rawPort := parsed.Port(); rawPort != "" {
		port, err = strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return mihomoProxy{}, fmt.Errorf("invalid VLESS port")
		}
	}
	query := parsed.Query()
	if query.Get("security") != "reality" || query.Get("pbk") == "" {
		return mihomoProxy{}, fmt.Errorf("VLESS URI does not contain REALITY settings")
	}
	network := query.Get("type")
	if network != "" && network != "tcp" {
		return mihomoProxy{}, fmt.Errorf("unsupported VLESS transport")
	}
	serverName := query.Get("sni")
	if serverName == "" {
		serverName = host
	}
	fingerprint := query.Get("fp")
	if fingerprint == "" {
		fingerprint = "chrome"
	}
	return mihomoProxy{
		Name: name, Server: host, Port: port, UUID: uuid,
		Flow: query.Get("flow"), ServerName: serverName,
		ClientFingerprint: fingerprint, PublicKey: query.Get("pbk"), ShortID: query.Get("sid"),
	}, nil
}

func writeMihomoProxy(output *bytes.Buffer, proxy mihomoProxy) {
	fmt.Fprintf(output, `  - name: %s
    type: vless
    server: %s
    port: %d
    uuid: %s
    udp: true
    flow: %s
    packet-encoding: xudp
    tls: true
    servername: %s
    client-fingerprint: %s
    reality-opts:
      public-key: %s
      short-id: %s
    encryption: ""
    network: tcp
`, yamlQuote(proxy.Name), yamlQuote(proxy.Server), proxy.Port, yamlQuote(proxy.UUID), yamlQuote(proxy.Flow),
		yamlQuote(proxy.ServerName), yamlQuote(proxy.ClientFingerprint), yamlQuote(proxy.PublicKey), yamlQuote(proxy.ShortID))
}

func yamlQuote(value string) string {
	return strconv.Quote(value)
}
