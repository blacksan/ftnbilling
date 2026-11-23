# V-SOL OLT and ONU: setting up TR-069 management

The fleet uses V-SOL ONUs. The OLT gives each ONU its TR-069 WAN through the ONU profile, and every ONU logs in to the ACS with the **shared** login first. On its first check-in the server writes a per-device login into the box (see "Security model" below).

Items marked **[UNVERIFIED]** come from SmartOLT and V-SOL blog pages, forum posts or memory, not from a V-SOL manual (the V1600G manuals could not be read) and not from our own hardware. Do each of them once on the bench with one ONU and write down what you saw before relying on it. Items marked **[VERIFIED IN CODE]** are what this server does and is tested for.

## What the server expects

- A management VLAN (99 by default) on the OLT uplink, tagged, towards the router. The router has `10.6.<n>.1/24` on it and a DHCP server `ftn-onu-dhcp` (pool `.10` to `.250`). The ONUs can reach the ACS (`10.99.0.1:7547`) and the router's NTP, and nothing else. No DNS is offered: the ACS URL is an address. **[VERIFIED IN CODE]**
- The ACS URL is `http://10.99.0.1:7547/` (shown on Settings, ONU management, with the shared user name and password).
- Periodic inform every 300 seconds. The server also sets it on the box.

## OLT steps

1. **Management VLAN.** Create the VLAN (99) on the OLT and allow it tagged on the uplink to the router. Leave it untagged or tagged on the PON side as the ONU profile says. **[UNVERIFIED]** whether the uplink should be tagged or untagged depends on the upstream switch; V-SOL's own notes say only that it depends on it.
2. **ONU profile for tenants.** Add a WAN of type TR069 on VLAN 99, addressing mode DHCP, with the service the ONU profile calls management. **[UNVERIFIED]** The exact V-SOL CLI or web-UI steps and menu names for the TR-069 WAN could not be read from any source. SmartOLT's published page says V-SOL ONUs provision TR-069 from the OLT automatically and other brands need manual WAN setup on the ONU page (a competitor's claim, medium-low confidence).
3. **TR-069 settings of the profile.** ACS URL, the shared user name and password from Settings, periodic inform on, 300 seconds. **[UNVERIFIED]** whether the V-SOL profile pushes these over OMCI or the ONU takes them from DHCP; the OLT is known to re-push the profile when the ONU registers again.
4. **Authentication by serial.** Keep serial-number authentication (SN-auth) on and turn auto-learn off for the production PON ports. A GPON serial that is not registered must not come online. V-SOL OLTs authenticate ONUs by GPON serial and auto-learn is on by default. This is the main defence against a stranger's box (see the first-claim note below).
5. **ONU isolation on VLAN 99.** Turn on ONU isolation (port isolation between ONUs) for the management VLAN, so one ONU cannot talk to another at layer 2. **[UNVERIFIED]** The V-SOL command or menu for this is not documented in any source we could read. The router side already has `arp=reply-only` on the VLAN and `add-arp=yes` on the DHCP server. **[VERIFIED IN CODE]** (the router check "ONU management network" warns when either is missing).
6. **Apply and watch.** Apply the profile. The ONU appears under ONUs within a few minutes, its serial is the GPON serial, and the house links by serial when the serial is in the register.

## DHCP options

- **Option 43 (ACS URL, sub-option 1) and option 60 (vendor class).** The router offers option 43 with sub-option 1 set to the ACS URL. **[UNVERIFIED]** that V-SOL ONUs read it. From memory: TR-111 CPEs send option 60 starting with "dslforum.org" and an ACS URL goes in option 43 sub-option 1. Confirm against the Broadband Forum TR-111 text and with one ONU. The profile on the OLT is the reliable way; option 43 is a bonus.
- **Option 82 (circuit-id, remote-id).** **[UNVERIFIED]** The V-SOL option 82 format (PON port, ONU ID, serial) is not documented in any source we could open. RouterOS 7.21 or later can bind a dynamic lease to option 82. If the values prove stable on a live OLT, binding the management lease to them would make the router side stronger than binding to the MAC. Until measured, the server binds an ONU to the router and the management MAC the router leased its address to.

## Security model (what the server does)

**[VERIFIED IN CODE]** (the fake-ONU tests in `internal/acs`):

- **Per-device login.** On first contact the server writes a random user name and password into the box (`ManagementServer.Username` and `Password`) and uses them from the next session. The shared login then only opens the first contact. If the box refuses (fault 9003 or 9008) the ONU is flagged "shared login only" on the list. That is a weaker identity, not an error.
- **Binding.** Each ONU is bound to its router and to the MAC its management address is leased to. A known serial that comes back on the **shared** login (a factory reset does that) is accepted only from the same router with its address leased to the recorded MAC. Anything else is **quarantined**: it gets no secrets and no tasks, the record is not moved, and the owner gets the alert "ONU <serial> checked in from an unexpected place: approve or block", with Approve and Block buttons on the web, the API and the Android app.
- **No silent moves.** A linked ONU is never moved to another router or address by itself. Three changes in an hour quarantine it.
- **Wi-Fi belongs to the house.** The Wi-Fi choice is stored on the account. A swapped, unlinked or returned box is unlinked at once, its register entry becomes "returned", and a factory reset is queued for its next check-in (the owner can cancel it). A returned box never receives a house's Wi-Fi. The new box gets the house's Wi-Fi on its first verified session.
- **Box web-admin password.** The server tries to set a random web-admin password on the box (TR-098 `UserInterface` and user-account objects, TR-181 `Device.Users.User.{i}.Password`) and keeps it encrypted, marked unverified. The owner can show it behind their own password. **[UNVERIFIED]** the parameter paths on V-SOL firmware; when the box does not support them the step is skipped silently. **Tenants must not be given the ONU admin login or its web page**: with it they can change what the server set, including the Wi-Fi security mode and the ACS address.
- **First claim.** An ONU serial the server has never seen is accepted on first contact with the shared login (the box has no other secret yet). A person who knows the shared login and a free address on the management VLAN could claim a serial before the real box shows up. Defences: SN-auth on the OLT (step 4), the PON port set to registered serials only, ONU isolation (step 5), and the alert when the real box later checks in from another place (it is quarantined). Keep the shared login out of tenants' hands.
- **TLS.** An HTTPS ACS is optional (`ACS_TLS_CERT` and `ACS_TLS_KEY`). The management path is a private VLAN and a WireGuard tunnel, so plain HTTP with Digest is the default. **[UNVERIFIED]** that V-SOL ONUs accept the certificate you use (many ONUs ship without a public CA list). Try one ONU before turning it on for a profile.

## Checks on the bench (write the results in the report)

1. Does a V-SOL ONU register on the OLT, take a DHCP address on VLAN 99 and check in with no manual step on the ONU page?
2. Does it refuse or accept the per-device login write (the list shows "shared login only" if it refuses)?
3. After a factory reset, does the OLT put the TR-069 profile back? Does the box check in again with `0 BOOTSTRAP` and the shared login, and is it restored (same router and MAC)?
4. Which Wi-Fi parameter names does it take (the model table under Settings, ONU management, fills from "It works")?
5. Is the WAN MAC the same as the label MAC? (Not known for V-SOL.)
6. Does option 43 work, and what do option 60 and option 82 contain?
7. Which parameter path takes a web-admin password?
