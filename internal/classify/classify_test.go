package classify

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		r    Reply
		want Category
	}{
		{"gmail rate", Reply{Code: 421, Enhanced: "4.7.28", Text: "Our system has detected an unusual rate of unsolicited mail"}, RateLimited},
		{"microsoft ip reputation throttle", Reply{Code: 451, Enhanced: "4.7.650", Text: "The mail server [203.0.113.1] has been temporarily rate limited due to IP reputation. (S775)"}, RateLimited},
		{"microsoft connections", Reply{Code: 451, Enhanced: "4.7.652", Text: "too many connections"}, RateLimited},
		{"yahoo tss04", Reply{Code: 421, Enhanced: "4.7.0", Tag: "TSS04", Text: "[TSS04] Messages from 203.0.113.1 temporarily deferred due to unexpected volume or user complaints"}, RateLimited},
		{"outlook rp-001", Reply{Code: 421, Tag: "RP-001", Text: "RP-001 The mail server IP connecting to Outlook.com has exceeded the rate limit"}, RateLimited},
		{"gmail generic 4.7.0 try again", Reply{Code: 421, Enhanced: "4.7.0", Text: "Try again later, closing connection"}, Busy},
		{"gmail low ip reputation 4.7.0", Reply{Code: 421, Enhanced: "4.7.0", Text: "This message has a very low reputation of the sending IP"}, RateLimited},
		{"microsoft s3150", Reply{Code: 550, Enhanced: "5.7.1", Tag: "S3150", Text: "Unfortunately, messages from [203.0.113.1] weren't sent. Please contact your Internet service provider since part of their network is on our block list (S3150)"}, ReputationBlock},
		{"yahoo tss11", Reply{Code: 553, Enhanced: "5.7.2", Tag: "TSS11", Text: "[TSS11] All messages from 203.0.113.1 will be permanently deferred"}, ReputationBlock},
		{"microsoft banned", Reply{Code: 550, Enhanced: "5.7.511", Text: "Access denied, banned sender"}, ReputationBlock},
		{"gmail unsolicited", Reply{Code: 550, Enhanced: "5.7.1", Text: "Our system has detected that this message is likely unsolicited mail"}, ReputationBlock},
		{"outlook 5.7.515", Reply{Code: 550, Enhanced: "5.7.515", Text: "Access denied, sending domain does not meet the required authentication level"}, AuthFailure},
		{"gmail 5.7.26", Reply{Code: 550, Enhanced: "5.7.26", Text: "This mail has been blocked because the sender is unauthenticated"}, AuthFailure},
		{"gmail 4.7.29 no tls", Reply{Code: 421, Enhanced: "4.7.29", Text: "Your email has been rate limited because it is unauthenticated"}, AuthFailure},
		{"gmail ptr 4.7.23", Reply{Code: 451, Enhanced: "4.7.23", Text: "The sending IP has no PTR record"}, AuthFailure},
		{"gmail 5.1.1", Reply{Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist"}, InvalidRecipient},
		{"user unknown no enhanced", Reply{Code: 550, Text: "User unknown in virtual mailbox table"}, InvalidRecipient},
		{"mailbox full", Reply{Code: 452, Enhanced: "4.2.2", Text: "The email account that you tried to reach is over quota"}, MailboxFull},
		{"dns", Reply{Code: 450, Enhanced: "4.4.3", Text: "Host or domain name not found"}, DNS},
		{"connection", Reply{Connection: true}, Connection},
		{"greeting block keeps category", Reply{Connection: true, Code: 554, Tag: "(BL)", Text: "554 IP=203.0.113.1 (BL)"}, ReputationBlock},
		{"microsoft 5.7.507 org block", Reply{Code: 550, Enhanced: "5.7.507", Text: "Access denied, [203.0.113.1] has been blocked by the recipient organization"}, ReputationBlock},
		{"microsoft 5.7.510 ipv6", Reply{Code: 550, Enhanced: "5.7.510", Text: "Access denied, the recipient does not accept email over IPv6"}, Policy},
		{"connect enhanced", Reply{Code: 450, Enhanced: "4.4.1", Text: "connect to mx[203.0.113.4]:25: Connection timed out"}, Connection},
		{"content", Reply{Code: 552, Enhanced: "5.6.0", Text: "message size exceeds"}, Content},
		{"busy", Reply{Code: 450, Enhanced: "4.2.1", Text: "The user you are trying to contact is receiving mail at a rate that prevents"}, Busy},
		{"greylist", Reply{Code: 450, Enhanced: "4.3.0", Text: "Greylisted, please try again"}, Busy},
		{"policy fallback", Reply{Code: 554, Enhanced: "5.7.9", Text: "something"}, Policy},
		{"delivered", Reply{Code: 250, Enhanced: "2.0.0", Text: "OK"}, Delivered},
		{"other", Reply{Code: 451, Enhanced: "4.9.9", Text: "x"}, Other},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.r); got != tc.want {
				t.Errorf("Classify(%+v) = %s, want %s", tc.r, got, tc.want)
			}
		})
	}
}

func TestTag(t *testing.T) {
	tests := []struct{ text, want string }{
		{"421 4.7.0 [TSS04] Messages from 203.0.113.1 temporarily deferred", "TSS04"},
		{"553 5.7.2 [TSS11] All messages permanently deferred", "TSS11"},
		{"550 5.7.1 part of their network is on our block list (S3150)", "S3150"},
		{"451 4.7.650 The mail server has been temporarily rate limited due to IP reputation. (S775)", "S775"},
		{"421 RP-001 The mail server IP connecting to Outlook.com has exceeded the rate limit", "RP-001"},
		{"550 5.7.1 [CS01] Message rejected due to local policy", "CS01"},
		{"421 4.7.0 Try again later, closing connection. (EHLO) a12-20020a056402 - gsmtp", ""},
		{"550-5.1.1 The email account that you tried to reach does not exist", ""},
		{"451 4.7.650 The mail server [203.0.113.1] has been temporarily rate limited due to IP reputation. (S775)", "S775"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := Tag(tc.text); got != tc.want {
			t.Errorf("Tag(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestProviderCodesFromTheirDocs(t *testing.T) {
	tests := []struct {
		name string
		code int
		enh  string
		text string
		want Category
	}{
		{"comcast dnsbl", 554, "", "imta: resimta-ch2-18v.sys.comcast.net ... found on one or more DNSBLs, see http://postmaster.comcast.net/smtp-error-codes.php#BL000001", ReputationBlock},
		{"comcast session limit", 452, "", "Too many emails sent on this session", RateLimited},
		{"comcast not our customer", 550, "", "Not our customer", InvalidRecipient},
		{"yahoo no account", 554, "", "delivery error: dd This user doesn't have a yahoo.com account (user5@yahoo.com) [-5] - mta123.mail.mud.yahoo.com", InvalidRecipient},
		{"yahoo no account postfix dsn", 554, "5.0.0", "host mta7.am0.yahoodns.net[203.0.113.7] said: 554 delivery error: dd This user doesn't have a yahoo.com account (user5@yahoo.com) [-5] - mta123.mail.mud.yahoo.com (in reply to end of DATA command)", InvalidRecipient},
		{"mailbox not found", 550, "", "Requested action not taken: mailbox not found", InvalidRecipient},
		{"recipient unknown", 550, "5.0.0", "Recipient unknown", InvalidRecipient},
		{"no account without a domain is not a recipient", 554, "", "Sender doesn't have a valid account", Other},
		{"sender domain not found is not a recipient", 450, "4.1.8", "Sender address rejected: Domain not found", Other},
		{"comcast ptr", 554, "", "PTR lookup failure", AuthFailure},
		{"comcast dmarc", 554, "", "DM000001 message rejected for DMARC policy", AuthFailure},
		{"spectrum rate", 550, "5.1.0", "sender rejected. Refer to Error Codes section at https://www.spectrum.net/support/internet/understanding-email-error-codes for more information. AUP#In-1310", RateLimited},
		{"spectrum block", 550, "5.1.0", "AUP#In-1000", ReputationBlock},
		{"cox connect", 421, "", "cox.net Connection refused AUP#CXCNCT", RateLimited},
		{"cox block", 554, "", "cox.net AUP#CXBL", ReputationBlock},
		{"t-online block", 554, "", "IP=203.0.113.1 - A problem occurred. (Ask your postmaster for help or to contact tosa@rx.t-online.de to clarify.) (BL)", ReputationBlock},
		{"t-online connections", 421, "", "Maximum parallel connections for your IP-Address reached", RateLimited},
		{"netease spam", 554, "", "DT:SPM 163 smtp10,DsCowABn...", Content},
		{"netease connections", 421, "", "HL:ICC the IP that sends mail has exceeded the connection limit", RateLimited},
		{"netease spf", 550, "", "MI:SPF mail from this ip is not allowed by the domain spf record", AuthFailure},
		{"qq frequency", 550, "", "Ip frequency limited. https://service.mail.qq.com/detail/122/171", RateLimited},
		{"gmail low domain reputation permanent", 550, "5.7.1", "This message has been blocked as likely suspicious due to the very low reputation of the sending domain", ReputationBlock},
		{"gmail low ip reputation temporary", 421, "4.7.0", "Our system has detected that this message is suspicious due to the very low reputation of the sending IP address", RateLimited},
		{"too many recipients per transaction", 452, "4.5.3", "Too many recipients", Busy},
		{"yahoo complaints", 554, "5.7.1", "[TSS09] All messages from 203.0.113.1 will be permanently deferred; Retrying will NOT succeed. blocked for user complaints", ReputationBlock},
		{"yahoo greylist", 421, "4.7.0", "[GL01] Message from (203.0.113.1) temporarily deferred - 4.16.50", Busy},
		{"gmail disabled account", 550, "5.2.1", "The email account that you tried to reach is disabled", InvalidRecipient},
		{"mail.ru spam", 550, "", "spam message rejected. Please visit https://help.mail.ru/notspam-support/id?c=...", ReputationBlock},
		{"free errors", 421, "", "Too many errors from your IP (203.0.113.1), please visit http://postmaster.free.fr/", Busy},
		{"free spams", 500, "", "Too many spams from your IP (203.0.113.1), please visit http://postmaster.free.fr/", ReputationBlock},
		{"la poste content", 550, "5.7.1", "LPN007_518 content rejected", Content},
		{"orange connections", 421, "", "mwinf5c45 ME Trop de connexions, veuillez verifier votre configuration. Too many connections, slow down. OFR004_104 [104]", RateLimited},
		{"orange dmarc", 550, "5.7.1", "Service refuse. Veuillez essayer plus tard. service refused, please try later. OFR_515 [515] OFR005_515", AuthFailure},
		{"orange blacklist", 550, "", "mwinf5c45 ME Adresse IP source bloquee pour incident de spam. Client host blocked for spamming issues. OFR006_102 [102]", ReputationBlock},
		{"gmx block listed", 554, "", "-gmx.net (mxgmx117) Nemesis ESMTP Service not available 554-IP address is block listed. 554 For explanation visit https://postmaster.gmx.net/en/case?c=r0201", ReputationBlock},
		{"exchange recipient", 550, "5.4.1", "Recipient address rejected: Access denied. AS(201806281)", InvalidRecipient},
		{"exchange bad helo", 550, "5.7.506", "Access Denied, Bad HELO", AuthFailure},
		{"exchange range limit", 550, "5.7.508", "Access denied, [203.0.113.1] has exceeded permitted limits within 5.7.508 range", RateLimited},
		{"exchange access denied later", 451, "4.7.850", "Access denied, please try again later", Busy},
		{"exchange banned ip", 550, "5.7.606", "Access denied, banned sending IP [203.0.113.1]", ReputationBlock},
		{"exchange dmarc", 550, "5.7.509", "Access denied, sending domain does not pass DMARC verification", AuthFailure},
		{"yandex target policy", 550, "5.7.1", "Policy rejection on the target address", InvalidRecipient},
		{"gmail unusual rate 5xx", 550, "5.7.28", "Our system has detected an unusual rate of unsolicited mail originating from your IP address", ReputationBlock},
		{"gmail unusual rate blocked 5.7.1", 550, "5.7.1", "Our system has detected an unusual rate of unsolicited mail originating from your IP address. To protect our users from spam, mail sent from your IP address has been blocked.", ReputationBlock},
		{"unusual rate 5xx no enhanced", 550, "", "Our system has detected an unusual rate of mail originating from your IP address", ReputationBlock},
		{"unusual rate 4xx no enhanced", 421, "", "Our system has detected an unusual rate of mail originating from your IP address", RateLimited},
		{"gmail policy prohibits", 550, "5.7.1", "The user or domain that you are sending to (or from) has a policy that prohibited the mail that you sent", Policy},
		{"gmail low reputation soft", 421, "4.7.0", "This message has a very low reputation of the sending IP, see https://support.google.com/mail/answer/188131", RateLimited},
		{"gmail recipient rate", 450, "4.2.1", "The user you are trying to contact is receiving mail at a rate that prevents additional messages from being delivered", Busy},
		{"yahoo ts03 permanent", 553, "5.7.1", "[TS03] All messages from 203.0.113.1 will be permanently deferred", ReputationBlock},
		{"microsoft server busy", 451, "4.7.500", "Server busy. Please try again later", Busy},
		{"fastmail spamhaus", 554, "5.7.1", "Service unavailable; Client host [203.0.113.1] blocked using xbl.spamhaus.org.rbl.local", ReputationBlock},
		{"naver spf", 550, "5.7.1", "Sender rejected because SPF check failed", AuthFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Reply{Code: tc.code, Enhanced: tc.enh, Tag: Tag(tc.text), Text: tc.text}
			if got := Classify(r); got != tc.want {
				t.Errorf("Classify(%d %s %q, tag %q) = %s, want %s", tc.code, tc.enh, tc.text, r.Tag, got, tc.want)
			}
		})
	}
}

func TestTagFormats(t *testing.T) {
	tests := []struct{ text, want string }{
		{"found on one or more DNSBLs, see http://postmaster.comcast.net/smtp-error-codes.php#BL000001", "BL000001"},
		{"Too many connections, slow down. OFR004_104 [104]", "OFR004_104"},
		{"sender rejected. AUP#In-1310", "AUP#IN-1310"},
		{"cox.net AUP#CXBL", "AUP#CXBL"},
		{"A problem occurred. (...) (BL)", "(BL)"},
		{"HL:ICC the IP that sends mail has exceeded the connection limit", "HL:ICC"},
		{"LPN007_518 content rejected", "LPN007_518"},
		{"Relay access denied SFR_IN_101", "SFR_IN_101"},
		{"250 2.0.0 OK 1710058502 x12si345678wrs.123 - gsmtp", ""},
		{"queue id S1234 is not a vendor tag", ""},
	}
	for _, tc := range tests {
		if got := Tag(tc.text); got != tc.want {
			t.Errorf("Tag(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
