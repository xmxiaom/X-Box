package main

// 常用歌手 / 组合 / 乐队名预置表。
//
// 用途：判断文件名里「哪一侧是歌手」时最直接的一票。
// 只作为证据之一：命中即高置信；未命中时仍靠「目录内重复度」「文件夹名」判断。
//
// 匹配规则：
//   - 归一化后比较（小写、去标点空格），所以 "S.H.E" 与 "she" 等价；
//   - 只对整个片段或合作歌手的分段做**精确**匹配，不做包含匹配
//     （"黎明前的黑暗" 不会命中 "黎明"）；
//   - 支持多人合作写法：顿号、逗号、斜杠、&、+、feat.、ft.、with。
//
// 表不追求穷尽，覆盖主流华语（80 年代至今）、港台、日韩、欧美即可；
// 冷门歌手仍由重复度与目录名证据兜底。

import "strings"

// 华语男歌手
var artistCNMale = []string{
	"周杰伦", "林俊杰", "陈奕迅", "张学友", "刘德华", "王力宏", "陶喆", "孙楠",
	"张信哲", "周华健", "李宗盛", "罗大佑", "张国荣", "谭咏麟", "黎明", "郭富城",
	"李克勤", "古巨基", "许志安", "苏永康", "郑中基", "陈百强", "黄家驹", "黄贯中",
	"黄家强", "叶世荣", "伍佰", "任贤齐", "张宇", "游鸿明", "庾澄庆", "齐秦",
	"费玉清", "张震岳", "潘玮柏", "罗志祥", "萧敬腾", "林宥嘉", "周传雄", "光良",
	"品冠", "曹格", "方大同", "陈楚生", "张杰", "薛之谦", "李荣浩", "毛不易",
	"华晨宇", "许嵩", "汪苏泷", "朴树", "许巍", "郑智化", "赵传", "刘欢",
	"韩磊", "腾格尔", "屠洪刚", "满文军", "沙宝亮", "杨坤", "刀郎", "庞龙",
	"胡彦斌", "林志炫", "张敬轩", "侧田", "吴克群", "蔡旻佑", "韦礼安", "李健",
	"老狼", "高晓松", "陈羽凡", "胡海泉", "窦唯", "张楚", "何勇", "汪峰",
	"郑钧", "丁武", "高旗", "谢天笑", "左小祖咒", "万晓利", "宋冬野", "马頔",
	"赵雷", "陈鸿宇", "尧十三", "陈升", "张雨生", "王杰", "童安格", "邰正宵",
	"高明骏", "巫启贤", "熊天平", "李玖哲", "张智成", "范逸臣", "萧煌奇", "黄品源",
	"李圣杰", "张洪量", "杨宗纬", "黄明志", "陈零九", "高尔宣", "瘦子", "周治平",
	"罗时丰", "洪荣宏", "叶启田", "蔡振南", "陈一郎", "王识贤", "翁立友", "蔡小虎",
	"文章", "张镐哲", "金城武", "杜德伟", "黄耀明", "刘以达", "林一峰", "陈辉阳",
	"林海峰", "葛民辉", "黄秋生", "郑伊健", "陈小春", "谢霆锋", "陈冠希", "余文乐",
	"方力申", "邓健泓", "梁汉文", "苏有朋", "吴奇隆", "陈志朋", "林志颖", "郑源",
	"欢子", "六哲", "冷漠", "祁隆", "王铮亮", "李代沫", "张赫宣", "梁博",
	"周深", "毛宁", "林依轮", "解晓东", "孙浩", "满江", "黄格选", "戴军",
	"火风", "尹相杰", "臧天朔", "崔健", "张岭", "汪苏泷", "后弦", "逃跑计划",
}

// 华语女歌手
var artistCNFemale = []string{
	"王菲", "邓丽君", "蔡依林", "张惠妹", "林忆莲", "梅艳芳", "陈慧琳", "杨千嬅",
	"容祖儿", "郑秀文", "梁静茹", "孙燕姿", "萧亚轩", "莫文蔚", "刘若英", "许茹芸",
	"辛晓琪", "万芳", "齐豫", "潘越云", "苏芮", "黄莺莺", "陈淑桦", "叶倩文",
	"彭羚", "李玟", "张韶涵", "王心凌", "杨丞琳", "田馥甄", "任家萱", "陈嘉桦",
	"戴佩妮", "范玮琪", "徐怀钰", "徐佳莹", "叮当", "张碧晨", "邓紫棋", "郁可唯",
	"张靓颖", "李宇春", "周笔畅", "何洁", "黄雅莉", "金莎", "胡杨林", "卓依婷",
	"韩红", "那英", "田震", "毛阿敏", "韦唯", "杭天琪", "李娜", "杨钰莹",
	"陈明", "甘萍", "周冰倩", "高胜美", "龙飘飘", "韩宝仪", "谢采妘", "叶丽仪",
	"徐小凤", "甄妮", "汪明荃", "张德兰", "陈美龄", "周蕙", "许美静", "柯以敏",
	"李翊君", "孟庭苇", "裘海正", "方季惟", "陈明真", "金智娟", "娃娃", "蔡幸娟",
	"潘美辰", "伊能静", "方芳", "张清芳", "曾淑勤", "阿悄", "本兮", "曲婉婷",
	"萨顶顶", "谭维维", "吴莫愁", "李佳薇", "黄龄", "金玟岐", "陈粒", "程璧",
	"花粥", "谢春花", "阿肆", "任然", "庄心妍", "云朵", "乌兰图雅", "降央卓玛",
	"斯琴格日乐", "德德玛", "王筝", "曹方", "叶蓓", "丁薇", "金海心", "纪如璟",
	"张浅潜", "姜昕", "陈绮贞", "张悬", "焦安溥", "雷光夏", "陈珊妮", "杨乃文",
	"顺子", "黄小琥", "彭佳慧", "林晓培", "戴爱玲", "高慧君", "纪晓君", "蔡健雅",
	"陈洁仪", "蔡淳佳", "郭美美", "黄湘怡", "何耀珊", "龚芝怡", "陈慧娴", "邝美云",
	"黎瑞恩", "汤宝如", "王馨平", "李蕙敏", "郑融", "关心妍", "吴雨霏", "邓丽欣",
	"泳儿", "傅颖", "杨爱瑾", "梁咏琪", "卢巧音", "何韵诗", "谢安琪", "卫兰",
	"卫诗", "吴若希", "菊梓乔", "谷娅溦", "谭嘉仪", "戴祖仪", "梁文音", "郭静",
	"卓文萱", "曾沛慈", "魏如萱", "张艾嘉", "刘惜君", "曾轶可", "黄英", "江映蓉",
	"李霄云", "苏运莹", "张钰琪", "希林娜依高", "单依纯", "黄霄雲", "阿兰", "陈雪凝",
	"王贰浪", "孟慧圆", "李佳薇", "刘美麟", "袁娅维", "吉克隽逸", "张含韵", "安又琪",
}

// 华语组合 / 乐队 / 合唱团
var artistCNGroups = []string{
	"Beyond", "五月天", "苏打绿", "小虎队", "红孩儿", "东方快车", "凡人二重唱", "优客李林",
	"南方二重唱", "锦绣二重唱", "无印良品", "动力火车", "羽泉", "水木年华", "彝人制造", "山鹰组合",
	"花儿乐队", "麦田守望者", "新裤子", "果味VC", "达达乐队", "便利商店", "卡奇社", "牛奶咖啡",
	"好妹妹乐队", "房东的猫", "凤凰传奇", "玖月奇迹", "至上励合", "男才女貌", "阿里郎", "黑鸭子",
	"女子十二乐坊", "芳华十八", "唐朝乐队", "黑豹乐队", "轮回乐队", "指南针乐队", "超载乐队", "战斧乐队",
	"面孔乐队", "太极乐队", "达明一派", "草蜢", "温拿乐队", "蓝战士", "软硬天师", "廿四味",
	"LMF", "农夫", "Shine", "Boy'z", "2R", "Cookies", "Twins", "S.H.E",
	"Sweety", "BY2", "蜜雪薇琪", "JS", "F.I.R", "飞儿乐团", "南拳妈妈", "飞轮海",
	"5566", "Energy", "棒棒堂", "黑涩会美眉", "信乐团", "四分卫", "脱拉库", "1976",
	"Tizzy Bac", "旺福", "轻松玩", "宇宙人", "麋先生", "八三夭", "先知玛莉", "灭火器",
	"董事长乐团", "闪灵乐团", "浊水溪公社", "交工乐队", "草东没有派对", "落日飞车", "老王乐队", "茄子蛋",
	"告五人", "顽童MJ116", "玖壹壹", "盘尼西林", "痛仰乐队", "二手玫瑰", "万能青年旅店", "反光镜",
	"九连真人", "五条人", "刺猬乐队", "后海大鲨鱼", "重塑雕像的权利", "声音玩具", "Joyside", "Carsick Cars",
	"木马乐队", "超级市场", "与非门", "杭盖乐队", "野孩子", "舌头乐队", "苍蝇乐队", "鲍家街43号",
	"子曰乐队", "逃跑计划", "旅行团", "海龟先生", "低苦艾", "马赛克乐队", "傻子与白痴", "Deca Joins",
	"Vast & Hazy", "甜梅号", "回声乐团", "法兰黛", "Hello Nico", "Cicada", "十二大美女", "芝麻龙眼",
}

// 日韩歌手 / 组合
var artistJKKR = []string{
	"宇多田光", "滨崎步", "安室奈美惠", "中岛美嘉", "大冢爱", "仓木麻衣", "久石让", "坂本龙一",
	"YUI", "MISIA", "米津玄师", "YOASOBI", "Ado", "椎名林檎", "东京事变", "彩虹乐队",
	"L'Arc~en~Ciel", "X Japan", "B'z", "Mr.Children", "南方之星", "Spitz", "生物股长", "岚",
	"关ジャニ∞", "King & Prince", "Snow Man", "乃木坂46", "欅坂46", "日向坂46", "AKB48", "早安少女组",
	"桃色幸运草Z", "Perfume", "BABYMETAL", "ONE OK ROCK", "RADWIMPS", "神思者", "喜多郎", "姬神",
	"宗次郎", "BTS", "BLACKPINK", "IU", "李知恩", "少女时代", "Super Junior", "EXO",
	"TWICE", "BigBang", "PSY", "Rain", "宝儿", "BoA", "东方神起", "SHINee",
	"f(x)", "Red Velvet", "NCT", "SEVENTEEN", "Stray Kids", "TXT", "ENHYPEN", "ITZY",
	"aespa", "NewJeans", "LE SSERAFIM", "IVE", "GIDLE", "MAMAMOO", "Apink", "Girl's Day",
	"AOA", "Sistar", "2NE1", "Wonder Girls", "KARA", "T-ara", "After School", "4Minute",
	"miss A", "Brown Eyed Girls", "白智英", "李孝利", "金泰妍", "郑秀妍", "林允儿", "裴秀智",
	"尹美莱", "朴孝信", "成诗京", "李承哲", "金光石", "申升勋", "金建模", "徐太志",
	"复活乐队", "紫雨林", "Nell", "脸红的思春期", "乐童音乐家", "AKMU", "10cm",
}

// 欧美歌手 / 组合
var artistWestern = []string{
	"Taylor Swift", "Adele", "Ed Sheeran", "Coldplay", "Maroon 5", "Linkin Park",
	"Michael Jackson", "Madonna", "Celine Dion", "Mariah Carey", "Whitney Houston", "Eagles",
	"Queen", "The Beatles", "Rolling Stones", "U2", "Nirvana", "Green Day",
	"Bon Jovi", "Guns N' Roses", "Metallica", "Radiohead", "Bruno Mars", "Rihanna",
	"Beyoncé", "Lady Gaga", "Katy Perry", "Ariana Grande", "Justin Bieber", "Justin Timberlake",
	"Eminem", "Jay-Z", "Snoop Dogg", "Billie Eilish", "Dua Lipa", "Sam Smith",
	"Charlie Puth", "Shawn Mendes", "Norah Jones", "Jason Mraz", "John Mayer", "Sting",
	"Phil Collins", "Enya", "Yanni", "Bandari", "Secret Garden", "Kevin Kern",
	"Backstreet Boys", "NSYNC", "Westlife", "Boyzone", "Spice Girls", "ABBA",
	"Bee Gees", "Pink Floyd", "Led Zeppelin", "AC/DC", "Aerosmith", "Red Hot Chili Peppers",
	"Foo Fighters", "Oasis", "Blur", "Suede", "Pulp", "Muse",
	"Travis", "Keane", "Snow Patrol", "The Cranberries", "The Cardigans", "Roxette",
	"Ace of Base", "Aqua", "Michael Learns to Rock", "Air Supply", "Carpenters", "Elvis Presley",
	"Frank Sinatra", "Bob Dylan", "Leonard Cohen", "John Lennon", "Paul McCartney", "David Bowie",
	"Prince", "Stevie Wonder", "Ray Charles", "Otis Redding", "Aretha Franklin", "Diana Ross",
	"Tina Turner", "Cher", "Barbra Streisand", "Josh Groban", "Andrea Bocelli", "Sarah Brightman",
	"Enigma", "Era", "Ludovico Einaudi", "Yiruma", "Richard Clayderman", "Maksim",
	"Imagine Dragons", "OneRepublic", "The Chainsmokers", "Calvin Harris", "Avicii", "Martin Garrix",
	"Alan Walker", "Kygo", "Zedd", "David Guetta", "Marshmello", "The Weeknd",
	"Post Malone", "Drake", "Kanye West", "Kendrick Lamar", "Cardi B", "Nicki Minaj",
	"Selena Gomez", "Demi Lovato", "Miley Cyrus", "Camila Cabello", "Halsey", "Lana Del Rey",
	"Lorde", "Sia", "P!nk", "Kelly Clarkson", "Christina Aguilera", "Britney Spears",
	"Jennifer Lopez", "Shakira", "Ricky Martin", "Enrique Iglesias", "Julio Iglesias",
}

// 纯音乐 / 新世纪 / 影视配乐
var artistInstrumental = []string{
	"林海", "石进", "久石让", "坂本龙一", "喜多郎", "神思者", "班得瑞", "神秘园",
	"雅尼", "凯文柯恩", "理查德克莱德曼", "马克西姆", "Yiruma", "Ludovico Einaudi", "Enigma", "Era",
}

// commonArtistSet 归一化后的歌手名集合（各分类合并去重）
var commonArtistSet = func() map[string]bool {
	all := [][]string{artistCNMale, artistCNFemale, artistCNGroups, artistJKKR, artistWestern, artistInstrumental}
	m := make(map[string]bool)
	for _, g := range all {
		for _, n := range g {
			if k := normalizeForCompare(n); k != "" {
				m[k] = true
			}
		}
	}
	return m
}()

// artistNameCount 预置歌手名总数（归一化去重后），供测试与诊断
func artistNameCount() int { return len(commonArtistSet) }

// isKnownArtist 判断是否命中常用歌手名（支持多人合作写法）
func isKnownArtist(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if commonArtistSet[normalizeForCompare(s)] {
		return true
	}
	// 合作歌手：顿号 / 逗号 / 斜杠 / & / ＋ / 分号 / 竖线
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '、', ',', '，', '/', '&', '＆', '+', '；', ';', '|':
			return true
		}
		return false
	})
	for _, f := range fields {
		if k := normalizeForCompare(strings.TrimSpace(f)); k != "" && commonArtistSet[k] {
			return true
		}
	}
	// feat. / ft. / with 前后任一侧命中即可
	low := strings.ToLower(s)
	for _, sep := range []string{" feat.", " feat ", " ft.", " ft ", "(feat", " with "} {
		i := strings.Index(low, sep)
		if i < 0 {
			continue
		}
		head := strings.TrimSpace(s[:i])
		tail := strings.Trim(strings.TrimSpace(s[i+len(sep):]), " .:)(")
		if head != "" && isKnownArtist(head) {
			return true
		}
		if tail != "" && isKnownArtist(tail) {
			return true
		}
	}
	return false
}
