import { describe, expect, it } from "vitest";
import { turnRelayLabel } from "./turnLabel";

describe("turnRelayLabel", () => {
  it("labels relay candidates with the TURN server address and protocol", () => {
    expect(
      turnRelayLabel({ candidateType: "relay", address: "1.2.3.4", relayProtocol: "udp" }),
    ).toBe("via TURN 1.2.3.4 (udp)");
    expect(
      turnRelayLabel({ candidateType: "relay", address: "5.6.7.8", relayProtocol: "tcp" }),
    ).toBe("via TURN 5.6.7.8 (tcp)");
  });
  it("falls back when relay fields are missing", () => {
    expect(turnRelayLabel({ candidateType: "relay" })).toBe("via TURN (?)");
    expect(turnRelayLabel({ candidateType: "relay", address: "1.2.3.4" })).toBe("via TURN 1.2.3.4 (udp)");
  });
  it("labels non-relay (direct) paths", () => {
    expect(turnRelayLabel({ candidateType: "host", address: "192.168.1.5" })).toBe("direct");
    expect(turnRelayLabel({ candidateType: "srflx", address: "1.1.1.1" })).toBe("direct");
  });
  it("dash when no candidate yet", () => {
    expect(turnRelayLabel(undefined)).toBe("—");
  });
});
