// The email the founders get for each message the form keeps: plain text, sent through Resend's API
// (https://resend.com/docs/api-reference/emails/send-email) with the secret RESEND_API_KEY (made with sending access only). It goes
// to NOTIFY_TO from NOTIFY_FROM (vars in wrangler.jsonc); the sender's domain must be verified in that Resend account.
//
// The form calls this after the message is stored, in ctx.waitUntil, so the visitor's answer never waits on it or
// depends on it: a notice that cannot be sent is logged (`npx wrangler tail`) and the message is still in KV.

// Text a visitor wrote may hold anything. The subject is one header line, so it loses every control character;
// the body is text/plain (never HTML), so it keeps line breaks and tabs and loses the rest. Both lose the invisible
// and direction-changing characters that would make a line read differently from what it holds.
const LINE = /[\u0000-\u001f\u007f-\u009f\u200b\u200c\u200e\u200f\u2028-\u202e\u2060-\u2069\ufeff]+/g;
const BODY = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u200b\u200c\u200e\u200f\u2028-\u202e\u2060-\u2069\ufeff]+/g;
// Reply-To is set only for an address of the plainest shape; any other address is still in the body.
const PLAIN_ADDRESS = /^[A-Za-z0-9._%+'-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$/;

// The notice for a stored message (`record` as kept in KV); `topics` maps a topic to the words the page shows.
export function notice(record, topics) {
  const line = (s) => s.replace(LINE, " ").trim();
  const body = (s) => s.replace(/\r\n?/g, "\n").replace(BODY, " ");
  const topic = topics[record.topic] ?? record.topic;
  const site = line(record.site ?? "potluck.sh");
  const subject = Array.from(line(`${site}: ${record.name} (${topic})`)).slice(0, 150).join("");
  const text = [
    `From:    ${line(record.name)} <${line(record.email)}>`,
    `Topic:   ${line(topic)}`,
    `Team:    ${line(record.team) || "(none given)"}`,
    `At:      ${record.at}${record.country ? ` from ${record.country}` : ""}`,
    "",
    body(record.message),
    "",
    "--",
    `Sent by the contact form on ${site}. Reply to this email to answer the sender.`,
    "Every message is also kept in KV: GET https://potluck.sh/api/contact (site/README.md).",
  ].join("\n");
  const out = { subject, text };
  if (PLAIN_ADDRESS.test(record.email)) out.replyTo = record.email;
  return out;
}

export const RESEND_URL = "https://api.resend.com/emails";

// Sends the notice; never throws. Without the key (local dev, or before it is set) nothing is sent.
export async function sendNotice(env, record, topics) {
  if (typeof env.RESEND_API_KEY !== "string" || env.RESEND_API_KEY === "") {
    console.warn("contact: no RESEND_API_KEY; message stored, no email sent");
    return;
  }
  if (typeof env.NOTIFY_FROM !== "string" || env.NOTIFY_FROM === "") {
    console.error("contact: NOTIFY_FROM is not set; message stored, no email sent");
    return;
  }
  if (typeof env.NOTIFY_TO !== "string" || env.NOTIFY_TO === "") {
    console.error("contact: NOTIFY_TO is not set; message stored, no email sent");
    return;
  }
  const { subject, text, replyTo } = notice(record, topics);
  const email = { from: env.NOTIFY_FROM, to: [env.NOTIFY_TO], subject, text };
  if (replyTo) email.reply_to = replyTo;
  try {
    const res = await fetch(RESEND_URL, {
      method: "POST",
      headers: { authorization: `Bearer ${env.RESEND_API_KEY}`, "content-type": "application/json" },
      body: JSON.stringify(email),
    });
    if (!res.ok) console.error(`contact: notice not sent: Resend answered ${res.status} ${(await res.text()).slice(0, 300)}`);
  } catch (err) {
    console.error(`contact: notice not sent: ${err?.message ?? err}`);
  }
}
