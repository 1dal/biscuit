package main

import "fmt"
import "runtime"
import "sync"
import "sync/atomic"
import "time"
import "unsafe"

const (
	VENDOR	int	= 0x0
	DEVICE		= 0x02
	STATUS		= 0x06
	CLASS		= 0x0b
	SUBCLASS	= 0x0a
	HEADER		= 0x0e
	_BAR0		= 0x10
	_BAR1		= 0x14
	_BAR2		= 0x18
	_BAR3		= 0x1c
	_BAR4		= 0x20
)

// width is width of the register in bytes
func pci_read(tag pcitag_t, reg, width int) int {
	enable := 1 << 31
	rsh := reg % 4
	r := reg - rsh
	t := enable | int(tag) | r

	pci_addr := 0xcf8
	pci_data := 0xcfc
	runtime.Outl(pci_addr, t)
	d := runtime.Inl(pci_data)
	runtime.Outl(pci_addr, 0)

	ret := int(uint(d) >> uint(rsh*8))
	m := ((1 << (8*uint(width))) - 1)
	return ret & m
}

func pci_write(tag pcitag_t, reg, val int) {
	if reg & 3 != 0 {
		panic("reg must be 32bit aligned")
	}
	enable := 1 << 31
	t := enable | int(tag) | reg

	pci_addr := 0xcf8
	pci_data := 0xcfc
	runtime.Outl(pci_addr, t)
	runtime.Outl(pci_data, val)
	runtime.Outl(pci_addr, 0)
}

// don't forget to enable busmaster in pci command reg before attaching
func pci_bar_pio(tag pcitag_t, barn int) uintptr {
	if barn < 0 || barn > 4 {
		panic("bad bar #")
	}
	ret := pci_read(tag, _BAR0 + 4*barn, 4)
	ispio := 1
	if ret & ispio == 0 {
		panic("is memory bar")
	}
	return uintptr(ret &^ 0x3)
}

// some memory bars include size in the low bits; this method doesn't mask such
// bits out.
func pci_bar_mem(tag pcitag_t, barn int) (uintptr, int) {
	if barn < 0 || barn > 4 {
		panic("bad bar #")
	}
	bari := _BAR0 + 4*barn
	ret := pci_read(tag, bari, 4)
	ispio := 1
	if ret & ispio != 0 {
		panic("is port io bar")
	}
	pci_write(tag, bari, -1)
	blen := uint32(pci_read(tag, bari, 4))
	blen &^= 0xf
	blen = ^blen + 1
	if blen == 0 {
		panic("bad bar length")
	}
	pci_write(tag, bari, ret)
	mtype := (uint32(ret) >> 1) & 0x3
	if mtype == 1 {
		// 32bit memory bar
		return uintptr(ret &^ 0xf), int(blen)
	}
	if mtype != 2 {
		panic("weird memory bar type")
	}
	if barn > 4 {
		panic("64bit memory bar requires 2 bars")
	}
	ret2 := pci_read(tag, bari + 4, 4)
	return uintptr((ret2 << 32) | ret &^ 0xf), int(blen)
}

func pci_dump() {
	pcipr := func(b, dev, f int, ind bool) (int, bool) {
		t := mkpcitag(b, dev, f)
		v  := pci_read(t, VENDOR, 2)
		if v == 0xffff {
			return 0, false
		}
		d   := pci_read(t, DEVICE, 2)
		mf  := pci_read(t, HEADER, 1)
		cl  := pci_read(t, CLASS, 1)
		scl := pci_read(t, SUBCLASS, 1)
		if ind {
			fmt.Printf("    ")
		}
		fmt.Printf("%d: %d: %d: %#x %#x (%#x %#x)\n", b, dev, f, v, d,
		    cl, scl)
		return mf, true
	}
	fmt.Printf("PCI dump:\n")
	for b := 0; b < 256; b++ {
		for dev := 0; dev < 32; dev++ {
			mf, ok := pcipr(b, dev, 0, false)
			if !ok {
				continue
			}
			if mf & 0x80 != 0 {
				for f := 1; f < 8; f++ {
					pcipr(b, dev, f, true)
				}
			}
		}
	}
}

func pcibus_attach() {
	pciinfo := func(b, dev, f int) (int, int, bool, bool) {
		t := mkpcitag(b, dev, f)
		v  := pci_read(t, VENDOR, 2)
		if v == 0xffff {
			return 0, 0, false, false
		}
		d  := pci_read(t, DEVICE, 2)
		mf := pci_read(t, HEADER, 1)
		ismf := mf & 0x80 != 0
		return v, d, ismf, true
	}
	devattach := func(b, dev int) {
		vid, did, mf, ok := pciinfo(b, dev, 0)
		if !ok {
			return
		}
		pci_attach(vid, did, b, dev, 0)
		if !mf {
			return
		}
		// attach multi functions too
		for f := 1; f < 8; f++ {
			vid, did, _, ok := pciinfo(b, dev, f)
			if !ok {
				continue
			}
			pci_attach(vid, did, b, dev, f)
		}
	}
	for b := 0; b < 256; b++ {
		for dev := 0; dev < 32; dev++ {
			devattach(b, dev)
		}
	}
}

type pcitag_t uint

func mkpcitag(b, d, f int) pcitag_t {
	return pcitag_t(b << 16 | d << 11 | f << 8)
}

func breakpcitag(b pcitag_t) (int, int, int) {
	bus := int((b >> 16) & 0xff)
	d := int((b >> 11) & 0x1f)
	f := int((b >> 8) & 0x7)
	return bus, d, f
}

func pci_attach(vendorid, devid, bus, dev, fu int) {
	PCI_VEND_INTEL := 0x8086
	PCI_DEV_PIIX3 := 0x7000
	PCI_DEV_3400  := 0x3b20
	PCI_DEV_X540T := 0x1528

	// map from vendor ids to a map of device ids to attach functions
	alldevs := map[int]map[int]func(int, int, pcitag_t) {
		PCI_VEND_INTEL : {
			PCI_DEV_PIIX3 : attach_piix3,
			PCI_DEV_3400 : attach_3400,
			PCI_DEV_X540T: attach_x540t,
			},
		}

	tag := mkpcitag(bus, dev, fu)
	devs, ok := alldevs[vendorid]
	if !ok {
		return
	}
	attach, ok := devs[devid]
	if !ok {
		return
	}
	attach(vendorid, devid, tag)
}

func attach_piix3(vendorid, devid int, tag pcitag_t) {
	if disk != nil {
		panic("adding two disks")
	}
	IRQ_DISK = 14
	INT_DISK = IRQ_BASE + IRQ_DISK

	d := &legacy_disk_t{}
	d.init(0x1f0, 0x3f6)
	disk = d
	fmt.Printf("legacy disk attached\n")
}

func attach_3400(vendorid, devid int, tag pcitag_t) {
	if disk != nil {
		panic("adding two disks")
	}

	intline := 0x3d
	pin := pci_read(tag, intline, 1)
	if pin < 1 || pin > 4 {
		panic("bad PCI pin")
	}

	// map PCI pin to IOAPIC pin number. the Intel PCH chipset exposes this
	// mapping through PCI registers and memory mapped IO so we don't need
	// to parse AML (thank you flying spaghetti monster!)
	taglpc := mkpcitag(0, 31, 0)
	rcba_p := pci_read(taglpc, 0xf0, 4)
	if rcba_p & 1 == 0 {
		panic("no root complex base")
	}
	rcba_p &^= ((1 << 14) - 1)
	// memory reads/writes to RCBA must be 32bit aligned
	rcba := dmaplen32(uintptr(rcba_p), 0x342c)
	// PCI dev 31 PIRQ routes
	routes := rcba[0x3140/4]
	pirq := (routes >> (4*(uint32(pin) - 1))) & 0x7
	// Intel PCH's IOAPIC has PIRQs on input pins 16-24
	gsi := int(16 + pirq)

	// make sure chipset isn't steering this PCI interrupt to the 8259
	// (which we have disabled)
	proutereg := 0x60
	if pirq >= 4 {
		proutereg = 0x68
	}
	v := pci_read(taglpc, proutereg, 4)
	disable := 0x80
	v |= disable << ((pirq % 4)*8)
	pci_write(taglpc, proutereg, v)

	IRQ_DISK = gsi
	INT_DISK = IRQ_BASE + IRQ_DISK

	d := &pciide_disk_t{}
	// 3400's PCI-native IDE command/control block
	rbase := pci_bar_pio(tag, 0)
	allstats := pci_bar_pio(tag, 1)
	busmaster := pci_bar_pio(tag, 4)

	d.init(rbase, allstats, busmaster)
	disk = d
	fmt.Printf("3400: base %#x, cntrl: %#x, bm: %#x, irq: %d\n", rbase,
	    allstats, busmaster, gsi)
}

type disk_t interface {
	start(*idebuf_t, bool)
	complete([]uint8, bool)
	intr() bool
	int_clear()
}

// use ata pio for fair comparisons against xv6, but i want to use ahci (or
// something) eventually. unlike xv6, we always use disk 0
const(
	ide_bsy = 0x80
	ide_drdy = 0x40
	ide_df = 0x20
	ide_err = 0x01

	ide_cmd_read = 0x20
	ide_cmd_write = 0x30
)

func ide_init(rbase uintptr) bool {
	ide_wait(rbase, false)

	found := false
	for i := 0; i < 1000; i++ {
		r := int(runtime.Inb(uint16(rbase + 7)))
		if r == 0xff {
			fmt.Printf("floating bus!\n")
			break
		} else if r != 0 {
			found = true
			break
		}
	}
	if found {
		fmt.Printf("IDE disk detected\n");
		return true
	}

	fmt.Printf("no IDE disk\n");
	return false
}

func ide_wait(base uintptr, chk bool) bool {
	var r int
	c := 0
	for {
		r = int(runtime.Inb(uint16(base + 7)))
		if r & (ide_bsy | ide_drdy) == ide_drdy {
			break
		}
		c++
		if c > 10000000 {
			fmt.Printf("waiting a very long time for disk...\n")
			c = 0
		}
	}
	if chk && r & (ide_df | ide_err) != 0 {
		return false
	}
	return true
}

func idedata_ready(base uintptr) {
	c := 0
	for {
		drq := 1 << 3
		st := int(runtime.Inb(uint16(base + 7)))
		if st & drq != 0 {
			return
		}
		c++
		if c > 10000000 {
			fmt.Printf("waiting a long time for DRQ...\n")
		}
	}
}

func ide_start(rbase, allstatus uintptr, ibuf *idebuf_t, writing bool) {
	ireg := func(n uintptr) uint16 {
		return uint16(rbase + n)
	}
	ide_wait(rbase, false)
	outb := runtime.Outb
	outb(uint16(allstatus), 0)
	outb(ireg(2), 1)
	bn := ibuf.block
	bd := ibuf.disk
	outb(ireg(3), uint8(bn & 0xff))
	outb(ireg(4), uint8((bn >> 8) & 0xff))
	outb(ireg(5), uint8((bn >> 16) & 0xff))
	outb(ireg(6), uint8(0xe0 | ((bd & 1) << 4) | (bn >> 24) & 0xf))
	if writing {
		outb(ireg(7), ide_cmd_write)
		idedata_ready(rbase)
		runtime.Outsl(int(ireg(0)), unsafe.Pointer(&ibuf.data[0]),
		    512/4)
	} else {
		outb(ireg(7), ide_cmd_read)
	}
}

func ide_complete(base uintptr, dst []uint8, writing bool) {
	if !writing {
		// read sector
		if ide_wait(base, true) {
			runtime.Insl(uint16(base + 0),
			    unsafe.Pointer(&dst[0]), 512/4)
		}
	} else {
		// cache flush; only needed for old disks?
		//runtime.Outb(base + 7, 0xe7)
	}
}

type legacy_disk_t struct {
	rbase	uintptr
	allstat	uintptr
}

func (d *legacy_disk_t) init(base, allst uintptr) {
	d.rbase = base
	d.allstat = allst
	ide_init(d.rbase)
}

func (d *legacy_disk_t) start(ibuf *idebuf_t, writing bool) {
	ide_start(d.rbase, d.allstat, ibuf, writing)
}

func (d *legacy_disk_t) complete(dst []uint8, writing bool) {
	ide_complete(d.rbase, dst, writing)
}

func (d *legacy_disk_t) intr() bool {
	return true
}

func (d *legacy_disk_t) int_clear() {
	// read status so disk clears int
	runtime.Inb(uint16(d.rbase + 7))
	runtime.Inb(uint16(d.rbase + 7))
	irq_eoi(IRQ_DISK)
}

type pciide_disk_t struct {
	rbase	uintptr
	allstat	uintptr
	bmaster uintptr
}

func (d *pciide_disk_t) init(base, allst, busmaster uintptr) {
	d.rbase = base
	d.allstat = allst
	d.bmaster = busmaster
	ide_init(d.rbase)
}

func (d *pciide_disk_t) start(ibuf *idebuf_t, writing bool) {
	ide_start(d.rbase, d.allstat, ibuf, writing)
}

func (d *pciide_disk_t) complete(dst []uint8, writing bool) {
	ide_complete(d.rbase, dst, writing)
}

func (d *pciide_disk_t) intr() bool {
	streg := uint16(d.bmaster + 0x02)
	bmintr := uint(1 << 2)
	st := runtime.Inb(streg)
	if st & bmintr == 0 {
		return false
	}
	return true
}

func (d *pciide_disk_t) int_clear() {
	// read status so disk clears int
	runtime.Inb(uint16(d.rbase + 7))
	runtime.Inb(uint16(d.rbase + 7))

	// in PCI-native mode, clear the interrupt via the legacy bus master
	// base, bar 4.
	streg := uint16(d.bmaster + 0x02)
	st := runtime.Inb(streg)
	er := uint(1 << 1)
	if st & er != 0 {
		panic("disk error")
	}
	runtime.Outb(streg, uint8(st))

	// and via apic
	irq_eoi(IRQ_DISK)
}

type _oride_t struct {
	src	int
	dst	int
	// trigger sense
	level	bool
	// polarity
	low	bool
}

type acpi_ioapic_t struct {
	base		uintptr
	overrides	map[int]_oride_t
}

func _acpi_cksum(tbl []uint8) {
	var cksum uint8
	for _, c := range tbl {
		cksum += c
	}
	if cksum != 0 {
		panic("bad ACPI table checksum")
	}
}

// returns a slice of the requested table and whether it was found
func _acpi_tbl(rsdt []uint8, sig string) ([]uint8, bool) {
	// RSDT contains 32bit pointers, XSDT contains 64bit pointers.
	hdrlen := 36
	ptrs := rsdt[hdrlen:]
	var tbl []uint8
	for len(ptrs) != 0 {
		tbln := uintptr(readn(ptrs, 4, 0))
		ptrs = ptrs[4:]
		tbl = dmaplen(tbln, 8)
		if string(tbl[:4]) == sig {
			l := readn(tbl, 4, 4)
			tbl = dmaplen(tbln, l)
			return tbl, true
		}
	}
	return nil, false
}

// returns number of cpus, IO physcal address of IO APIC, and whether both the
// number of CPUs and an IO APIC were found.
func _acpi_madt(rsdt []uint8) (int, acpi_ioapic_t, bool) {
	// find MADT table
	tbl, found := _acpi_tbl(rsdt, "APIC")
	var apicret acpi_ioapic_t
	if !found {
		return 0, apicret, false
	}
	_acpi_cksum(tbl)
	apicret.overrides = make(map[int]_oride_t)
	marrayoff := 44
	ncpu := 0
	elen := 1
	// m is array of "interrupt controller structures" in MADT
	for m := tbl[marrayoff:]; len(m) != 0; m = m[m[elen]:] {
		// ACPI 5.2.12.2: each processor is required to have a LAPIC
		// entry
		tlapic    := uint8(0)
		tioapic   := uint8(1)
		toverride := uint8(2)

		tiosapic := uint8(6)
		tlsapic := uint8(7)
		tpint := uint8(8)
		if m[0] == tlapic {
			flags := readn(m, 4, 4)
			enabled := 1
			if flags & enabled != 0 {
				ncpu++
			}
		} else if m[0] == tioapic {
			apicret.base = uintptr(readn(m, 4, 4))
			//fmt.Printf("IO APIC addr: %x\n", apicret.base)
			//fmt.Printf("IO APIC IRQ start: %v\n", readn(m, 4, 8))
		} else if m[0] == toverride {
			src := readn(m, 1, 3)
			dst := readn(m, 4, 4)
			v := readn(m, 2, 8)
			var nover _oride_t
			nover.src = src
			nover.dst = dst
			//var active string
			switch (v & 0x3) {
			case 0:
				//active = "conforms"
				if dst < 16 {
					nover.low = true
				} else {
					nover.low = false
				}
			case 1:
				//active = "high"
				nover.low = false
			case 2:
				//active = "RESERVED?"
				panic("bad polarity")
			case 3:
				//active = "low"
				nover.low = true
			}
			//var trig string
			switch ((v & 0xc) >> 2) {
			case 0:
				//trig = "conforms"
				if dst < 16 {
					nover.level = false
				} else {
					nover.level = true
				}
			case 1:
				//trig = "edge"
				nover.level = false
			case 2:
				//trig = "RESERVED?"
				panic("bad trigger mode")
			case 3:
				//trig = "level"
				nover.level = true
			}
			apicret.overrides[dst] = nover
			//fmt.Printf("IRQ OVERRIDE: %v -> %v (%v, %v)\n", src,
			//    dst, trig, active)
		} else if m[0] == tiosapic {
			//fmt.Printf("*** IO SAPIC\n")
		} else if m[0] == tlsapic {
			//fmt.Printf("*** LOCAL SAPIC\n")
		} else if m[0] == tpint {
			//fmt.Printf("*** PLATFORM INT\n")
		}
	}
	return ncpu, apicret, ncpu != 0 && apicret.base != 0
}

// returns false if ACPI claims that MSI is broken
func _acpi_fadt(rsdt []uint8) bool {
	tbl, found := _acpi_tbl(rsdt, "FACP")
	if !found {
		return false
	}
	_acpi_cksum(tbl)
	flags := readn(tbl, 2, 109)
	nomsi      := 1 << 3
	return flags & nomsi == 0
}

func _acpi_scan() ([]uint8, bool) {
	// ACPI 5.2.5: search for RSDP in EBDA and BIOS read-only memory
	ebdap := (0x40 << 4) | 0xe
	p := dmap8(ebdap)
	ebda := uintptr(readn(p, 2, 0))
	ebda <<= 4

	isrsdp := func(d []uint8) bool {
		s := string(d[:8])
		if s != "RSD PTR " {
			return false
		}
		var cksum uint8
		for i := 0; i < 20; i++ {
			cksum += d[i]
		}
		if cksum != 0 {
			return false
		}
		return true
	}
	rsdplen := 36
	for i := uintptr(0); i < 1 << 10; i += 16 {
		p = dmaplen(ebda + i, rsdplen)
		if isrsdp(p) {
			return p, true
		}
	}
	for bmem := uintptr(0xe0000); bmem < 0xfffff; bmem += 16 {
		p = dmaplen(bmem, rsdplen)
		if isrsdp(p) {
			return p, true
		}
	}
	return nil, false
}

func acpi_attach() int {
	rsdp, ok := _acpi_scan()
	if !ok {
		panic("no RSDP")
	}
	rsdtn := uintptr(readn(rsdp, 4, 16))
	//xsdtn := readn(rsdp, 8, 24)
	rsdt := dmaplen(rsdtn, 8)
	if rsdtn == 0 || string(rsdt[:4]) != "RSDT" {
		panic("no RSDT")
	}
	rsdtlen := readn(rsdt, 4, 4)
	rsdt = dmaplen(rsdtn, rsdtlen)
	// verify RSDT checksum
	_acpi_cksum(rsdt)
	// may want to search XSDT, too
	ncpu, ioapic, ok := _acpi_madt(rsdt)
	if !ok {
		panic("no cpu count")
	}

	apic.apic_init(ioapic)

	msi := _acpi_fadt(rsdt)
	if !msi {
		panic("no MSI")
	}

	return ncpu
}

/* x86 interrupts -- a truly horrifying nightmare
 *
 * braindumping here to remind myself later.
 *
 * TERMINOLOGY:
 *
 * Polarity: the electical state of the wire which the device causes when said
 *     device wants an interrupt. active-high or active-low.
 * Trigger mode/sense: how long a device will put the wire to the interrupt
 *     polarity. edge-triggered or level-triggered. an interrupt controller
 *     must be told how the devices use each interrupt line i.e. the polarity
 *     and trigger mode of each line.
 * PIC/8259: legacy interrupt controllers from the original PC-AT from 1984. an
 *     8259 only has 8 input pins. thus, in order to handle all 16 of the
 *     legacy interrupts, two 8259s were setup "cascaded" (the output of the
 *     "slave" is connected to IRQ 2 of the "master"). thus both 8259s are
 *     programmed independently.
 * IRQ: interrupt line connected to an 8259 (not IOAPIC). on some modern
 *     southbridges, IRQs are actually implemented by a single wire using
 *     "serial IRQs". IRQs are edge-triggered, active-high. IRQs cannot be
 *     shared with PCI interrupts because PCI interrupts are level-triggered
 *     and active-low. however, some southbridges allow PCI interrupts to be
 *     "steered" (converted) to IRQs and take care of converting the
 *     polarity/trigger-mode of the wire.
 * PIRQ: PCI interrupt line. level-triggered and active-low.
 * PCI interrupt pins: each PCI device has four pins: INT[A-D]#. each pin can
 *     be connected to a different IOAPIC input pin or PCI link device. thus
 *     the OS must determine the mapping of the PCI interrupt pin used by a
 *     device to the IOAPIC input pin. this is the main challenge of x86
 *     interrupts.
 * PCI link device: programmable interrupt router. it routes PCI interrupt pins
 *     to different IOAPIC or 8259 input pins.
 * PIC mode: one of the two legacy modes stipulated by MP specification. in
 *     this mode, the 8259s are connected directly to the BSP, bypassing the
 *     BSPs LAPIC.
 * Virtual Wire mode: the other of the two legacy modes stipulated by the MP
 *     specification. in this mode the 8259s are connected to LINT0 (an
 *     interrupt pin) of the BSP (not bypassing the LAPIC as in PIC mode).
 * Symmetric mode: a mode stipulated by the MP specification where IOAPICs are
 *     used instead of 8259s which can deliver interrupts to any CPU, not just
 *     the BSP.
 * 8259 mode: ACPI's name for the mode that a PC starts in when booting (in MP
 *     spec terminology, either PIC mode or Virtual Wire mode).
 * APIC mode: ACPI's name for the mode where the PC uses IOAPICs instead of
 *     8259s ("Symmetric mode" in MP spec terminology). the OS tells ACPI that
 *     the system is switching to APIC mode by executing the _PIC method with
 *     argument 1 (one).
 * Global system interrupts: ACPI terminology for the distinct IDs assigned to
 *     each interrupt. ACPI provides a table (MADT) that contains the number of
 *     input pins and the global system interrupt start for each IOAPIC, thus
 *     the OS can figure out the mapping of global system interrupts to IOAPIC
 *     input pins.
 * Interrupt source override: information contained in ACPI's MADT table that
 *     instructs the OS how an IRQ maps to an IOAPIC input pin (the IOAPIC pin
 *     number, polarity, and trigger mode).
 *
 * HOW THE OS MAPS PCI INTERRUPT PINS TO IOAPIC INPUT PINS
 *
 * 1. the system boots in PIC or virtual wire mode. the BIOS has arbitrarily
 * routed interrupts from PCI devices to IRQs via PCI link devices. ACPI is
 * thinks the system is in 8259 mode.
 *
 * 2. OS decides to use IOAPICs instead of 8259s. the OS disables 8259s via
 * IMCR register (see MP spec).
 *
 * 3. OS executes ACPI _PIC method with argument value of "1" to ensure that
 * ACPI methods return information concerning IOAPICs and not 8259s.
 *
 * 4. for each PCI device's chosen PCI interrupt pin, the OS iterates through
 * the objects returned by the _PRT (PCI routing table) method on the
 * corresponding bus node in the ACPI device tree looking for the entry for
 * this (PCI dev number, PCI pin) pair. the resulting object describes either
 * the IOAPIC pin or the PCI link device to which the PCI pin connects. if the
 * former, the task is done.
 *
 * 5. the OS uses the ACPI _STA, _DIS, _CRS, and _SRS methods (status, disable,
 * current resources setting, set resource setting) on the PCI link device to
 * determine/configure the IOAPIC pin to which this PCI link device routes.
 *
 * steps 3-5 require an AML interpreter!
 *
 * luckily, my hardware exposes these PCI link devices through chipset PCI
 * device registers and memory mapped IO (in fact the ACPI methods in step 5
 * are implemented using these PCI device registers). thus i can avoid writing
 * an AML interpreter.
 *
 * if possible, i would like to use message signaled interrupts (MSI) -- the
 * documentation makes them seem much, much simpler. i think MSI avoids the
 * need for PCI interrupt pin to IOAPIC pin mapping entirely. however, i'll
 * need to upgrade my PIO disk driver to AHCI first since my SATA controller
 * won't generate MSI intterupts in IDE mode. also, we will still need the
 * IOAPIC to handle IRQs (COM1, keyboard, etc.).
*/

var apic apic_t

type apic_t struct {
	regs struct {
		sel	*uint32
		win	*uint32
		eoi	*uint32
	}
	npins	int
	// spinlock to protect access to the IOAPIC registers. because writing
	// an IOAPIC register requires two distinct memory writes, a single
	// IOAPIC register write cannot be atomic with respect to other memory
	// IOAPIC register writes. we must use a spinlock instead of a mutex
	// because we need to acquire this lock in interrupt context too.
	_mlock	runtime.Spinlock_t
}

func (ap *apic_t) apic_init(aioapic acpi_ioapic_t) {
	// enter "symmetric IO mode" (MP Conf 3.6.2); disable 8259 via IMCR
	runtime.Outb(0x22, 0x70)
	runtime.Outb(0x23, 1)

	base := aioapic.base
	va := dmaplen32(base, 4)
	ap.regs.sel = &va[0]

	va = dmaplen32(base + 0x10, 4)
	ap.regs.win = &va[0]

	va = dmaplen32(base + 0x40, 4)
	ap.regs.eoi = &va[0]

	pinlast := (apic.reg_read(1) >> 16) & 0xff
	ap.npins = int(pinlast + 1)

	bspid := uint32(bsp_apic_id)

	//fmt.Printf("APIC ID:  %#x\n", apic.reg_read(0))
	for i := 0; i < apic.npins; i++ {
		w1 := 0x10 + i*2
		r1 := apic.reg_read(w1)
		// vector: 32 + pin number
		r1 |= 32 + uint32(i)
		var islevel bool
		var islow bool
		if i < 16 {
			// ISA interrupts (IRQs) are edge-triggered, active
			// high
			islevel = false
			islow = false
		} else {
			// PIRQs are level-triggered, active-low (PCH 5.9.6 and
			// 5.10.2)
			islevel = true
			islow = true
		}
		// unless ACPI specifies differently via an "interrupt source
		// override"
		if ovr, ok := aioapic.overrides[i]; ok {
			islevel = ovr.level
			islow = ovr.low
		}
		level := uint32(1 << 15)
		activelow := uint32(1 << 13)
		if islevel {
			r1 |= level
		} else {
			r1 &^= level
		}
		if islow {
			r1 |= activelow
		} else {
			r1 &^= activelow
		}
		// delivery mode: fixed, destination mode: physical
		logical := uint32(1 << 11)
		r1 &^= logical
		dmode := uint32(7 << 8)
		r1 &^= dmode
		mask := uint32(1 << 16)
		r1 |= mask
		apic.reg_write(w1, r1)

		// route to BSP
		w2 := w1 + 1
		r2 := apic.reg_read(w2)
		r2 |= bspid << 24
		apic.reg_write(w2, r2)
	}
	//ap.dump()
}

func (ap *apic_t) reg_read(reg int) uint32 {
	if reg &^ 0xff != 0 {
		panic("bad IO APIC reg")
	}
	c := uint32(reg)
	runtime.Splock(&ap._mlock)
	runtime.Store32(ap.regs.sel, c)
	v := atomic.LoadUint32(ap.regs.win)
	runtime.Spunlock(&ap._mlock)
	return v
}

func (ap *apic_t) reg_write(reg int, v uint32) {
	if reg &^ 0xff != 0 {
		panic("bad IO APIC reg")
	}
	c := uint32(reg)
	runtime.Splock(&ap._mlock)
	runtime.Store32(ap.regs.sel, c)
	runtime.Store32(ap.regs.win, v)
	runtime.Spunlock(&ap._mlock)
}

func (ap *apic_t) irq_unmask(irq int) {
	if irq < 0 || irq > ap.npins {
		panic("bad irq")
	}

	mreg := 0x10 + irq*2
	v := ap.reg_read(mreg)
	maskbit := uint32(1 << 16)
	v &^= maskbit
	ap.reg_write(mreg, v)
}

// XXX nosplit because called from trapstub. this can go away when we have a
// LAPIC that supports EOI broadcast suppression.
//go:nosplit
func (ap *apic_t) irq_mask(irq int) {
	if irq < 0 || irq > ap.npins {
		runtime.Pnum(0xbad2)
		for {}
	}

	runtime.Splock(&ap._mlock)
	mreg := uint32(0x10 + irq*2)

	runtime.Store32(ap.regs.sel, mreg)
	v := atomic.LoadUint32(ap.regs.win)

	maskbit := uint32(1 << 16)
	v |= maskbit

	runtime.Store32(ap.regs.sel, mreg)
	runtime.Store32(ap.regs.win, v)
	runtime.Spunlock(&ap._mlock)
}

// LAPIC's are configured to broadcast EOI to IOAPICs for level-triggered
// interrupts automatically. newer CPUs let you disable EOI broadcast.
func (ap *apic_t) eoi(irq int) {
	if irq &^ 0xff != 0 {
		panic("bad irq")
	}
	runtime.Store32(ap.regs.eoi, uint32(irq + 32))
}

func (ap *apic_t) dump() {
	if ap.npins == 0 {
		return
	}
	for i := 0; i < ap.npins; i++ {
		r1 := ap.reg_read(0x10 + i*2)
		r2 := ap.reg_read(0x10 + i*2 + 1)
		intv := uint64(r2) << 32 | uint64(r1)
		vec := intv & 0xff
		m := intv & (1 << 16) != 0
		t := "edge"
		act := "high"
		if intv & (1 << 13) != 0 {
			act = "low"
		}
		if intv & (1 << 15) != 0 {
			t = "level"
		}
		delivs := map[uint64]string{0:"fixed", 1:"lowest priority",
		    2:"smi", 3:"reserved", 4:"NMI", 5:"INIT", 6:"reserved",
		    7:"ExtINIT"}
		deliv := delivs[((intv >> 8) & 3)]
		destmode := "physical"
		if intv & (1 << 11) != 0 {
			destmode = "logical"
		}
		dest := intv >> 56
		fmt.Printf("IRQ %v: vec: %v, mask: %v, mode: %v, " +
		    "act: %v, deliv: %v, destm: %v, dest: %#x\n", i, vec, m,
		    t, act, deliv, destmode, dest)
	}
}

type msivec_t uint

type msivecs_t struct {
	sync.Mutex
	avail	map[msivec_t]bool
}

var msivecs = msivecs_t{
	avail: map[msivec_t]bool { 56:true, 57:true, 58:true, 59:true, 60:true,
	    61:true, 62:true, 63:true},
}

// allocates an MSI interrupt vecber
func msi_alloc() msivec_t {
	msivecs.Lock()
	defer msivecs.Unlock()

	for i := range msivecs.avail {
		delete(msivecs.avail, i)
		return i
	}
	panic("no more MSI vecs")
}

func msi_free(vector msivec_t) {
	msivecs.Lock()
	defer msivecs.Unlock()

	if msivecs.avail[vector] {
		panic("double free")
	}
	msivecs.avail[vector] = true
}

// XXX use uncachable mappings for MMIO?
type x540reg_t uint
const (
	CTRL		x540reg_t	=    0x0
	// the x540 terminology is confusing regarding interrupts; an interrupt
	// is enabled when its bit is set in the mask set register (ims) and
	// disabled when cleared.
	CTRL_EXT			=    0x18
	EICR				=   0x800
	EIAC				=   0x810
	EICS				=   0x808
	EICS1				=   0xa90
	EICS2				=   0xa94
	EIMS				=   0x880
	EIMS1				=   0xaa0
	EIMS2				=   0xaa4
	EIMC				=   0x888
	EIMC1				=   0xab0
	EIMC2				=   0xab4
	EIAM				=   0x890
	GPIE				=   0x898
	EIAM1				=   0xad0
	EIAM2				=   0xad4
	PFVTCTL				=  0x51b0
	RTRPCS				=  0x2430
	RDRXCTL				=  0x2f00
	PFQDE				=  0x2f04
	RTRUP2TC			=  0x3020
	RTTUP2TC			=  0xc800
	DTXMXSZRQ			=  0x8100
	SECTXMINIFG			=  0x8810
	HLREG0				=  0x4240
	MFLCN				=  0x4294
	RTTDQSEL			=  0x4904
	RTTDT1C				=  0x4908
	RTTDCS				=  0x4900
	RTTPCS				=  0xcd00
	MRQC				=  0xec80
	MTQC				=  0x8120
	MSCA				=  0x425c
	MSRWD				=  0x4260
	LINKS				=  0x42a4
	DMATXCTL			=  0x4a80
	DTXTCPFLGL			=  0x4a88
	DTXTCPFLGH			=  0x4a8c
	EEMNGCTL			= 0x10110
	SWSM				= 0x10140
	SW_FW_SYNC			= 0x10160
	RXFECCERR0			=  0x51b8
	FCTRL				=  0x5080
	RXCSUM				=  0x5000
	RXCTRL				=  0x3000
	// statistic reg4sters
	SSVPC				=  0x8780
	GPTC				=  0x4080
	TXDGPC				=  0x87a0
	TPT				=  0x40d4
	PTC64				=  0x40d8
	PTC127				=  0x40dc
	MSPDC				=  0x4010
	XEC				=  0x4120
	BPTC				=  0x40f4
	FCCRC				=  0x5118
	B2OSPC				=  0x41c0
	B2OGPRC				=  0x2f90
	O2BGPTC				=  0x41c4
	O2BSPC				=  0x87b0
	CRCERRS				=  0x4000
	ILLERRC				=  0x4004
	ERRBC				=  0x4008
	GPRC				=  0x4074
	PRC64				=  0x405c
	PRC127				=  0x4060

	FLA				= 0x1001c
)

func _xreg(start, idx, max, step uint) x540reg_t {
	// XXX comment this out later so compiler can inline all these register
	// calculators
	if idx >= max {
		panic("bad x540 reg")
	}
	return x540reg_t(start + idx*step)
}

func template(n int) x540reg_t {
	return _xreg(0xa600, uint(n), 245, 4)
}

func FCRTH(n int) x540reg_t {
	return _xreg(0x3260, uint(n), 8, 4)
}

func RDBAL(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1000, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd000, uint(n)-64, 128-64, 0x40)
	}
}

func RDBAH(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1004, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd004, uint(n)-64, 128-64, 0x40)
	}
}

func RDLEN(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1008, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd008, uint(n)-64, 128-64, 0x40)
	}
}

func SRRCTL(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1014, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd014, uint(n)-64, 128-64, 0x40)
	}
}

func RXDCTL(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1028, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd028, uint(n)-64, 128-64, 0x40)
	}
}

func RDT(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1018, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd018, uint(n)-64, 128-64, 0x40)
	}
}

func RDH(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x1010, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd010, uint(n)-64, 128-64, 0x40)
	}
}

func QPRC(n int) x540reg_t {
	return _xreg(0x1030, uint(n), 16, 0x40)
}

func QPRDC(n int) x540reg_t {
	return _xreg(0x1430, uint(n), 16, 0x40)
}

func PFUTA(n int) x540reg_t {
	return _xreg(0xf400, uint(n), 128, 4)
}

func TDBAL(n int) x540reg_t {
	return _xreg(0x6000, uint(n), 128, 0x40)
}

func TDBAH(n int) x540reg_t {
	return _xreg(0x6004, uint(n), 128, 0x40)
}

func TDLEN(n int) x540reg_t {
	return _xreg(0x6008, uint(n), 128, 0x40)
}

func TDH(n int) x540reg_t {
	return _xreg(0x6010, uint(n), 128, 0x40)
}

func TDT(n int) x540reg_t {
	return _xreg(0x6018, uint(n), 128, 0x40)
}

func TXDCTL(n int) x540reg_t {
	return _xreg(0x6028, uint(n), 128, 0x40)
}

func TDWBAL(n int) x540reg_t {
	return _xreg(0x6038, uint(n), 128, 0x40)
}

func TDWBAH(n int) x540reg_t {
	return _xreg(0x603c, uint(n), 128, 0x40)
}

func RSCCTL(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x102c, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd02c, uint(n-64), 128-64, 0x40)
	}
}

func DCA_RXCTRL(n int) x540reg_t {
	if n < 64 {
		return _xreg(0x100c, uint(n), 64, 0x40)
	} else {
		return _xreg(0xd00c, uint(n-64), 128-64, 0x40)
	}
}

func RTRPT4C(n int) x540reg_t {
	return _xreg(0x2140, uint(n), 8, 4)
}

func RTTPT2C(n int) x540reg_t {
	return _xreg(0xdc20, uint(n), 8, 4)
}

func RTTDT2C(n int) x540reg_t {
	return _xreg(0x4910, uint(n), 8, 4)
}

func RXPBSIZE(n int) x540reg_t {
	return _xreg(0x3c00, uint(n), 8, 4)
}

func TXPBSIZE(n int) x540reg_t {
	return _xreg(0xcc00, uint(n), 8, 4)
}

func TXPBTHRESH(n int) x540reg_t {
	return _xreg(0x4950, uint(n), 8, 4)
}

func TQSM(n int) x540reg_t {
	return _xreg(0x8600, uint(n), 32, 4)
}

func IVAR(n int) x540reg_t {
	return _xreg(0x900, uint(n), 64, 4)
}

func EITR(n int) x540reg_t {
	if n < 24 {
		return _xreg(0x820, uint(n), 128, 4)
	} else {
		return _xreg(0x12300, uint(n-24), 128-24, 4)
	}
}

func PFVLVFB(n int) x540reg_t {
	return _xreg(0xf200, uint(n), 128, 4)
}

func VFTA(n int) x540reg_t {
	return _xreg(0xa000, uint(n), 128, 4)
}

func PFVFSPOOF(n int) x540reg_t {
	return _xreg(0x8200, uint(n), 8, 4)
}

func MPSAR(n int) x540reg_t {
	return _xreg(0xa600, uint(n), 256, 4)
}

func QPTC_L(n int) x540reg_t {
	return _xreg(0x8700, uint(n), 16, 8)
}

func QPTC(n int) x540reg_t {
	return _xreg(0x8680, uint(n), 16, 4)
}

func RAH(n uint) x540reg_t {
	return _xreg(0xa204, uint(n), 128, 8)
}

func RAL(n uint) x540reg_t {
	return _xreg(0xa200, uint(n), 128, 8)
}

// MDIO device is in bits [20:16] and MDIO reg is in [15:0]
type x540phyreg_t uint
const (
	// link status here (Auto-Negotiation Reserved Vendor Status 1)
	PHY_LINK	x540phyreg_t	= 0x07c810
	ALARMS1				= 0x1ecc00
)

type rxdesc_t struct {
	hwdesc *struct {
		p_data	uint64
		p_hdr	uint64
	}
	// saved buffer physical addresses since hardware overwrites them once
	// a packet is received
	p_pbuf	uint64
	p_hbuf	uint64
}

func (rd *rxdesc_t) init(pbuf, hbuf uintptr, hw *int) {
	rd.p_pbuf = uint64(pbuf)
	rd.p_hbuf = uint64(hbuf)
	rd.hwdesc = (*struct {
		p_data uint64
		p_hdr uint64
	})(unsafe.Pointer(hw))
}

func (rd *rxdesc_t) ready() {
	if (rd.p_pbuf | rd.p_hbuf) & 1 != 0 {
		panic("rx buffers must be word aligned")
	}
	rd.hwdesc.p_data = rd.p_pbuf
	rd.hwdesc.p_hdr = rd.p_hbuf
}

func (rd *rxdesc_t) rxdone() bool {
	dd := uint64(1)
	// compiler barrier
	return atomic.LoadUint64(&rd.hwdesc.p_hdr) & dd != 0
}

func (rd *rxdesc_t) eop() bool {
	eop := uint64(1 << 1)
	return rd.hwdesc.p_hdr & eop != 0
}

func (rd *rxdesc_t) pktlen() int {
	return int((rd.hwdesc.p_hdr >> 32) & 0xffff)
}

func (rd *rxdesc_t) hdrlen() int {
	return int((rd.hwdesc.p_data >> 21) & 0x3ff)
}

type txdesc_t struct {
	hwdesc *struct {
		p_addr	uint64
		rest	uint64
	}
	p_buf	uint64
	bufsz	uint64
	ctxt	bool
}

func (td *txdesc_t) init(p_addr, len uintptr, hw *int) {
	td.p_buf = uint64(p_addr)
	td.bufsz = uint64(len)
	td.hwdesc = (*struct {
		p_addr	uint64
		rest	uint64
	})(unsafe.Pointer(hw))
}

func (td *txdesc_t) ctxt_ipv4(_machlen, _ip4len int) {
	td.ctxt = true
	maclen := uint64(_machlen)
	td.hwdesc.p_addr = maclen << 9
	hlen := uint64(_ip4len)
	td.hwdesc.p_addr |= hlen
	// DTYP = 0010b
	td.hwdesc.rest = 0x2 << 20
	// DEXT = 1
	td.hwdesc.rest |= 1 << 29
	// TUCMD.IPV4 = 1
	ipv4 := uint64(1 << 12)
	td.hwdesc.rest |= ipv4
	// leave IDX zero
}

func (td *txdesc_t) ctxt_tcp(_machlen, _ip4len int) {
	td.ctxt = true
	maclen := uint64(_machlen)
	td.hwdesc.p_addr = maclen << 9
	hlen := uint64(_ip4len)
	td.hwdesc.p_addr |= hlen
	// DTYP = 0010b
	td.hwdesc.rest = 0x2 << 20
	// DEXT = 1
	td.hwdesc.rest |= 1 << 29
	// TUCMD.L4T = 1 (TCP)
	tcp := uint64(1 << 11)
	td.hwdesc.rest |= tcp
}

// returns remaining bytes
func (td *txdesc_t) data_continue(src [][]uint8) [][]uint8 {
	if len(src) == 0 {
		panic("empty buf")
	}

	td.ctxt = false
	dst := dmaplen(uintptr(td.p_buf), int(td.bufsz))
	paylen := uint64(0)
	for len(dst) != 0 && len(src) != 0 {
		cursrc := src[0]
		did := copy(dst, cursrc)
		paylen += uint64(did)
		dst = dst[did:]
		src[0] = cursrc[did:]
		if len(src[0]) == 0 {
			src = src[1:]
		}
	}

	// does this descriptor contain the end of the packet?
	last := false
	if len(src) == 0 {
		last = true
	}

	td.hwdesc.p_addr = td.p_buf
	// DTYP = 0011b
	td.hwdesc.rest = 0x3 << 20
	// DEXT = 1
	td.hwdesc.rest |= 1 << 29
	rs   := uint64(1 << 27)
	eop  := uint64(1 << 24)
	ifcs := uint64(1 << 25)
	td.hwdesc.rest |= ifcs
	if last {
		td.hwdesc.rest |= eop | rs
	}
	td._dtalen(paylen)
	return src
}

// returns remaining bytes
func (td *txdesc_t) mkraw(src [][]uint8, tlen int) [][]uint8 {
	ret := td.data_continue(src)
	td._paylen(uint64(tlen))
	return ret
}

// offloads ipv4 checksum calculation to the NIC. returns the remaining bytes.
func (td *txdesc_t) mkipv4(src [][]uint8, tlen int) [][]uint8 {
	ret := td.data_continue(src)
	td._paylen(uint64(tlen))
	// POPTS.IXSM = 1
	ixsm := uint64(1 << 40)
	td.hwdesc.rest |= ixsm
	// no need for index or CC
	return ret
}

// offloads IPv4/TCP checksum to the NIC. returns the remaining bytes.
func (td *txdesc_t) mktcp(src [][]uint8, tlen int) [][]uint8 {
	ret := td.mkipv4(src, tlen)
	// POPTS.TXSM = 1
	txsm := uint64(1 << 41)
	td.hwdesc.rest |= txsm
	return ret
}

func (td *txdesc_t) _dtalen(v uint64) {
	mask := uint64(0xffff)
	if v &^ mask != 0 || v == 0 {
		panic("bad dtalen")
	}
	td.hwdesc.rest &^= mask
	td.hwdesc.rest |= v
}

func (td *txdesc_t) _paylen(v uint64) {
	mask := uint64(0x3ffff)
	if v &^ mask != 0 || v == 0 {
		panic("bad paylen")
	}
	td.hwdesc.rest &^= mask << 46
	td.hwdesc.rest |= v << 46
}

func (td *txdesc_t) txdone() bool {
	rs   := uint64(1 << 27)
	// rs is reserved after writeback...
	if td.hwdesc.rest & rs == 0 {
		panic("dd may set only when rs is set")
	}
	dd := uint64(1 << 32)
	return td.hwdesc.rest & dd != 0
}

// if hw may writeback this descriptor, wait until the CPU observes the hw's
// write
func (td *txdesc_t) wbwait() {
	rs   := uint64(1 << 27)
	// rs is reserved after writeback...
	// compiler barrier
	if atomic.LoadUint64(&td.hwdesc.rest) & rs != 0 {
		for !td.txdone() {
			waits++
		}
	}
}

type x540_t struct {
	tag	pcitag_t
	bar0	[]uint32
	_locked	bool
	tx struct {
		// this lock protecting tx queue descriptors must be acquired
		// after driver locks are acquired.
		sync.Mutex
		cond	*sync.Cond
		ndescs	uint32
		descs	[]txdesc_t
	}
	rx struct {
		ndescs	uint32
		descs	[]rxdesc_t
		pkt	[][]uint8
	}
	pgs	int
	linkup	bool
	// big-endian
	mac	mac_t
	ip	ip4_t
	mtu	int
}

func (x *x540_t) init(t pcitag_t) {
	x.tag = t

	bar0, l := pci_bar_mem(t, 0)
	x.bar0 = dmaplen32(bar0, l)

	x.tx.cond = sync.NewCond(&x.tx)
	x.rx.pkt = make([][]uint8, 0, 30)

	x.mtu = 1500

	v := pci_read(t, 0x4, 2)
	memen := 1 << 1
	if v & memen == 0 {
		panic("memory access disabled")
	}
	busmaster := 1 << 2
	if v & busmaster == 0 {
		panic("busmaster disabled")
	}
	pciebmdis := uint32(1 << 2)
	y := x.rl(CTRL)
	if y & pciebmdis != 0 {
		panic("pcie bus master disable set")
	}
	nosnoop_en := 1 << 11
	v = pci_read(t, 0xa8, 2)
	if v & nosnoop_en == 0 {
		panic("pcie no snoop disabled")
	}
}

func (x *x540_t) rs(reg x540reg_t, val uint32) {
	if reg % 4 != 0 {
		panic("bad reg")
	}
	runtime.Store32(&x.bar0[reg/4], val)
}

func (x *x540_t) rl(reg x540reg_t) uint32 {
	if reg % 4 != 0 {
		panic("bad reg")
	}
	return atomic.LoadUint32(&x.bar0[reg/4])
}

func (x *x540_t) log(fm string, args ...interface{}) {
	b, d, f := breakpcitag(x.tag)
	s := fmt.Sprintf("X540:(%v:%v:%v): %s\n", b, d, f, fm)
	fmt.Printf(s, args...)
}

func (x *x540_t) _reset() {
	// if there is any chance that DMA may race with _reset, we must modify
	// _reset to execute the master disable protocol in (5.2.4.3.2)

	// link reset + device reset
	lrst := uint32(1 << 3)
	rst :=  uint32(1 << 26)
	v := x.rl(CTRL)
	v |= rst
	v |= lrst
	x.rs(CTRL, v)
	// 8.2.4.1.1: wait 1ms before checking reset bit after setting
	<- time.After(time.Millisecond)
	for x.rl(CTRL) & rst != 0 {
	}
	// x540 doc 4.6.3.2: wait for 10ms after reset "to enable a
	// smooth initialization flow"
	<- time.After(10*time.Millisecond)
}

func (x *x540_t) _int_disable() {
	maskall := ^uint32(0)
	x.rs(EIMC, maskall)
	x.rs(EIMC1, maskall)
	x.rs(EIMC2, maskall)
}

func (x *x540_t) _phy_read(preg x540phyreg_t) uint16 {
	if preg &^ ((1 << 21) - 1) != 0 {
		panic("bad phy reg")
	}
	mdicmd := uint32(1 << 30)
	// wait for MDIO to be ready
	for x.rl(MSCA) & mdicmd != 0 {
	}
	opaddr := uint32(0)
	phyport := uint32(0)
	v := uint32(preg) | phyport << 21 | opaddr << 26 | mdicmd
	x.rs(MSCA, v)
	for x.rl(MSCA) & mdicmd != 0 {
	}
	opread := uint32(3)
	v = uint32(preg) | phyport << 21 | opread << 26 | mdicmd
	x.rs(MSCA, v)
	for x.rl(MSCA) & mdicmd != 0 {
	}
	ret := x.rl(MSRWD)
	return uint16(ret >> 16)
}

var lockstat struct {
	sw	int
	hw	int
	fw	int
	nvmup	int
	swmng	int
}

// acquires the "lock" protecting the semaphores. returns whether fw timedout
func (x *x540_t) _reg_acquire() bool {
	to := 3*time.Second
	st := time.Now()
	smbi := uint32(1 << 0)
	for x.rl(SWSM) & smbi != 0 {
		if time.Since(st) > to {
			panic("SWSM timeout!")
		}
	}
	var fwdead bool
	st = time.Now()
	regsmp := uint32(1 << 31)
	for x.rl(SW_FW_SYNC) & regsmp != 0 {
		if time.Since(st) > to {
			x.log("SW_FW_SYNC timeout!")
			fwdead = true
			break
		}
	}
	return fwdead
}

func (x *x540_t) _reg_release() {
	regsmp := uint32(1 << 31)
	x.rs(SW_FW_SYNC, x.rl(SW_FW_SYNC) &^ regsmp)
	x.rs(SWSM, 0)
}

// takes all semaphores protecting NIC's NVM, PHY 0/1, and shared MAC registers
// from concurrent access by software and firmware
func (x *x540_t) hwlock() {
	if x._locked {
		panic("two hwlocks")
	}
	for i := 0; i < 100; i++ {
		if x._hwlock() {
			x._locked = true
			return
		}
		<- time.After(10*time.Millisecond)
	}
	fmt.Printf("lock stats: %v\n", lockstat)
	panic("hwlock timedout")
}

// returns true if the called acquired the software/firmware semaphore
func (x *x540_t) _hwlock() bool {
	// 11.7.5; this semaphore protects NVM, PHY[01], and MAC shared regs
	fwdead := x._reg_acquire()

	//sw_nvm  := uint32(1 << 0)
	//sw_phy0 := uint32(1 << 1)
	//sw_phy1 := uint32(1 << 2)
	//sw_mac  := uint32(1 << 3)
	hw_nvm  := uint32(1 << 4)
	fw_nvm  := uint32(1 << 5)
	fw_phy0 := uint32(1 << 6)
	fw_phy1 := uint32(1 << 7)
	fw_mac  := uint32(1 << 8)
	nvm_up  := uint32(1 << 9)
	sw_mng  := uint32(1 << 10)
	fwbits := fw_nvm | fw_phy0 | fw_phy1 | fw_mac

	ret := false
	v := x.rl(SW_FW_SYNC)
	if v & hw_nvm != 0 {
		lockstat.hw++
		goto out
	}
	if v & 0xf != 0 {
		lockstat.sw++
		goto out
	}
	if !fwdead && v & fwbits != 0 {
		lockstat.fw++
		goto out
	}
	if v & nvm_up != 0 {
		lockstat.nvmup++
	}
	if v & sw_mng != 0 {
		lockstat.swmng++
	}
	x.rs(SW_FW_SYNC, v | 0xf)
	ret = true
out:
	x._reg_release()
	return ret
}

func (x *x540_t) hwunlock() {
	if !x._locked {
		panic("not locked")
	}
	x._locked = false
	x._reg_acquire()
	v := x.rl(SW_FW_SYNC)
	// clear sw bits
	v &^= 0xf
	x.rs(SW_FW_SYNC, v)
	x._reg_release()
}

// returns linkup and link speed
func (x *x540_t) linkinfo() (bool, string) {
	link := uint32(1 << 30)
	v := x.rl(LINKS)
	speed := "unknown"
	switch (v >> 28) & 3 {
	case 1:
		speed = "100 Mb/s"
	case 2:
		speed = "1 Gb/s"
	case 3:
		speed = "10 Gb/s"
	}
	return v & link != 0, speed
}

func (x *x540_t) wait_linkup(secs int) bool {
	link := uint32(1 << 30)
	st := time.Now()
	s := time.Duration(secs)
	for {
		v := x.rl(LINKS)
		if v & link != 0 {
			return true
		}
		if time.Since(st) > s*time.Second {
			return false
		}
		runtime.Gosched()
	}
}

func (x *x540_t) pg_new() (*[512]int, uintptr) {
	x.pgs++
	a, _b := refpg_new()
	b := uintptr(_b)
	refup(b)
	return a, b
}

// returns after buf is enqueued to be trasmitted. buf's contents are copied to
// the DMA buffer, so buf's memory can be reused/freed
func (x *x540_t) tx_raw(buf [][]uint8) {
	x._tx_wait(buf, false, false, false)
}

func (x *x540_t) tx_ipv4(buf [][]uint8) {
	x._tx_wait(buf, true, false, false)
}

func (x *x540_t) tx_tcp(buf [][]uint8) {
	x._tx_wait(buf, true, true, false)
}

func (x *x540_t) tx_tcp_tso(buf [][]uint8) {
	x._tx_wait(buf, true, true, true)
}

func (x *x540_t) _tx_wait(buf [][]uint8, ipv4, tcp, tso bool) {
	x.tx.Lock()
	for !x._tx_enqueue(buf, ipv4, tcp, tso) {
		x.tx.cond.Wait()
	}
	x.tx.cond.Signal()
	x.tx.Unlock()
}

// caller must hold x.tx lock. returns true if buf was enqueued to be
// trasmitted.
func (x *x540_t) _tx_enqueue(buf [][]uint8, ipv4, tcp, tso bool) bool {
	if len(buf) == 0 {
		panic("wut")
	}
	end := x.rl(TDH(0))
	if end == 0 {
		end = x.tx.ndescs - 1
	} else {
		end--
	}
	tail := x.rl(TDT(0))
	// make sure there are enough tx descriptor to fit buf
	tlen := 0
	for i := 0; i < len(buf); {
		if len(buf[i]) == 0 {
			copy(buf[i:], buf[i+1:])
			buf = buf[:len(buf) - 1]
			continue
		}
		tlen += len(buf[i])
		i++
	}
	if tso {
		panic("no imp")
	}
	if tso && !tcp {
		panic("tso is only for tcp")
	}
	if tlen > 1500 && !tso {
		panic("should use tso")
	}
	if tlen == 0 {
		panic("wut")
	}
	need := tlen
	newtail := tail
	if ipv4 || tcp {
		// XXX the NIC only needs 1 context descriptor total for IPv4
		// checksums, not 1 per packet! (segmentation offload requires
		// 1 per packet)

		// first descriptor must be a context descriptor when using
		// checksum/segmentation offloading (only need 1 context for
		// all checksum offloads?)
		newtail = (newtail + 1) % x.tx.ndescs
	}
	for newtail != end && need != 0 {
		td := &x.tx.descs[newtail]
		bs := int(td.bufsz)
		if need <= bs {
			need = 0
		} else {
			need -= bs
		}
		newtail = (newtail + 1) % x.tx.ndescs
	}
	if need != 0 {
		// not enough room
		return false
	}
	// the first descriptors are special
	fd := &x.tx.descs[tail]
	fd.wbwait()
	if tcp {
		fd.ctxt_tcp(ETHERLEN, IP4LEN)
		tail = (tail + 1) % x.tx.ndescs
		fd = &x.tx.descs[tail]
		fd.wbwait()
		buf = fd.mktcp(buf, tlen)
	} else if ipv4 {
		fd.ctxt_ipv4(ETHERLEN, IP4LEN)
		tail = (tail + 1) % x.tx.ndescs
		fd = &x.tx.descs[tail]
		fd.wbwait()
		buf = fd.mkipv4(buf, tlen)
	} else {
		buf = fd.mkraw(buf, tlen)
	}
	tail = (tail + 1) % x.tx.ndescs
	for len(buf) != 0 {
		td := &x.tx.descs[tail]
		td.wbwait()
		buf = td.data_continue(buf)
		tail = (tail + 1) % x.tx.ndescs
	}
	if tail != newtail {
		panic("size miscalculated")
	}
	x.rs(TDT(0), newtail)
	return true
}

func (x *x540_t) rx_consume() {
	tailend := x.rl(RDH(0))
	if tailend == 0 {
		tailend = x.rx.ndescs - 1
	} else {
		tailend--
	}
	tail := x.rl(RDT(0))
	if tail == tailend {
		// queue is still full?
		spurs++
		return
	}
	// make sure the CPU observes the NIC's writeback of the RDH
	// descriptor; otherwise the CPU's and NIC's writes may race,
	// overwritting the CPU's (corrupting the rx descriptor)
	wd := &x.rx.descs[tailend]
	for !wd.rxdone() {
		waits++
	}
	// tail references an empty descriptor
	tail = (tail + 1) % x.rx.ndescs
	otail := tail
	tlen := 0
	pkt := x.rx.pkt[0:0]
	for {
		rd := &x.rx.descs[tail]
		if !rd.rxdone() {
			panic("wut?")
		}
		// packet may span multiple descriptors (only for RSC when MTU
		// less than descriptor DMA buffer size?)
		buf := dmaplen(uintptr(rd.p_pbuf), rd.pktlen())
		tlen += rd.pktlen()
		pkt = append(pkt, buf)
		if rd.eop() {
			net_start(pkt, tlen)
			numpkts++
			pkt = x.rx.pkt[0:0]
			tlen = 0
		}
		if tail == tailend {
			break
		}
		tail++
		if tail == x.rx.ndescs {
			tail = 0
		}
	}
	// only reset descriptors for full packets
	for {
		rd := &x.rx.descs[otail]
		if !rd.rxdone() {
			panic("wut?")
		}
		rd.ready()
		if otail == tail {
			break
		}
		otail++
		if otail == x.rx.ndescs {
			otail = 0
		}
	}
	x.rs(RDT(0), tail)
}

func (x *x540_t) int_handler(vector msivec_t) {
	rantest := false
	for {
		runtime.IRQsched(uint(vector))

		// interrupt status register clears on read
		st := x.rl(EICR)
		//x.log("*** NIC IRQ (%v) %#x", irqs, st)

		rx     := uint32(1 << 0)
		tx     := uint32(1 << 1)
		rxmiss := uint32(1 << 17)
		lsc    := uint32(1 << 20)

		// XXX code for polling instead of using interrupts
		//runtime.Gosched()
		//var st uint32
		//if !rantest {
		//	x.log("link wait...\n")
		//	if !x.wait_linkup(10) {
		//		x.log("NO LINK\n")
		//		return
		//	}
		//	x.log("GOT LINK\n")
		//	st |= lsc
		//}
		//rhead := x.rl(RDH(0))
		//thead := x.rl(TDH(0))
		//for st == 0 {
		//	if x.rl(RDH(0)) != rhead {
		//		st |= rx
		//	}
		//	if x.rl(TDH(0)) != thead {
		//		st |= tx
		//	}
		//}

		if st & lsc != 0 {
			// link status change
			up, speed := x.linkinfo()
			x.linkup = up
			if up {
				x.log("link up @ %s", speed)
			} else {
				x.log("link down")
			}
			if up && !rantest {
				// 18.26.5.49 (bhw)
				me := ip4_t(0x121a0531)
				x.ip = me
				nics[me] = x

				netmask := ip4_t(0xfffffe00)
				// 18.26.5.1
				gw := ip4_t(0x121a0401)
				routetbl.defaultgw(me, gw)
				net := me & netmask
				routetbl.insert_local(me, net, netmask)
				routetbl.routes.dump()

				rantest = true
				go x.tester1()
				//go x.tester2()
				//go x.tester3()
				//go x.tester4()
				//go x.tester5()
				//go x.tester6()
			}
		}
		if rxmiss & st != 0 {
			dropints++
		}
		if st & rx != 0 {
			// rearm rx descriptors
			x.rx_consume()
		}
		if st & tx != 0 {
			// wakeup threads waiting for tx queue buffer
			x.tx.Lock()
			x.tx.cond.Signal()
			x.tx.Unlock()
		}
	}
}

func (x *x540_t) tester1() {
	stirqs := irqs
	st := time.Now()
	for {
		<-time.After(30*time.Second)
		nirqs := irqs - stirqs
		drops  := x.rl(QPRDC(0))
		secs := time.Since(st).Seconds()
		pps := float64(numpkts) / secs
		ips := int(float64(nirqs) / secs)
		spursps := float64(spurs) / secs
		fmt.Printf("pkt %6v (%.4v/s), dr %v %v, ws %v, "+
		    "irqs %v (%v/s), spurs %v (%.3v/s)\n", numpkts, pps,
		    dropints, drops, waits, nirqs, ips, spurs, spursps)
	}
}

func (x *x540_t) tester2() {
	ai := ip4_t(0)
	lpr := time.Now()
	for {
		//<-time.After(2*time.Second)
		ai++
		if ai == 255 {
			ai = 1
		}
		dip := ip4_t(0x121a0500)
		dip += ai
		//dip := uint32(0x121a0530)
		st := time.Now()
		mac, err := arp_resolve(x.ip, dip)
		took := time.Since(st).Seconds()

		switch err {
		case 0:
			fmt.Printf("Resolved %s (%s) in %.3v s\n", ip2str(dip),
			    mac2str(mac[:]), took)
		case -ETIMEDOUT:
			fmt.Printf("Timeout for %s (%.3v s)\n", ip2str(dip),
			    took)
		default:
			panic("arp error")
		}
		if time.Since(lpr) > 7*time.Second {
			arptbl.Lock()
			fmt.Printf("ARP table:\n")
			now := time.Now()
			for ip, ar := range arptbl.m {
				mleft := ar.expire.Sub(now).Minutes()
				fmt.Printf("    %s -> %s (%.4v m)\n",
				    ip2str(ip), mac2str(ar.mac[:]), mleft)
			}
			lpr = time.Now()
			arptbl.Unlock()
		}
	}
}

func (x *x540_t) tester3() {
	local := true
	ai := ip4_t(0)
	for {
		local = !local
		//<-time.After(3*time.Second)

		target := ip4_t(0x121a0500)
		if local {
			ai++
			if ai == 255 {
				ai = 1
			}
			target += ai
			//target = ip4_t(0x121a0530)
		} else {
			target = ip4_t(0x08080808)
		}
		localip, routeip, err := routetbl.lookup(target)
		if err != 0 {
			panic("must succeed")
		}
		if localip != x.ip {
			panic("bad local")
		}

		dmac, err := arp_resolve(x.ip, routeip)
		if err == -ETIMEDOUT {
			continue
		} else if err != 0 {
			panic("no")
		}
		<-time.After(100*time.Millisecond)
		pingdata := make([]uint8, 8)
		writen(pingdata, 8, 0, int(time.Now().UnixNano()))
		var ping icmppkt_t
		pingtype := uint8(8)
		ping.init(&x.mac, dmac, x.ip, target, pingtype, pingdata)
		ping.crc()
		buf := [][]uint8{ping.hdrbytes(), pingdata}
		fmt.Printf("** ping to %s...\n", ip2str(target))
		x.tx_ipv4(buf)
	}
}

func (x *x540_t) tester4() {
	n := 0
	for {
		//mat := ip4_t(0x121a0413)
		bterm := ip4_t(0x121a0530)
		dport := uint16(31338 + n)
		//boss := ip4_t(0x9b622046)
		//dport := uint16(22)
		//if n % 3 == 1 {
		//	dip = ip4_t(0x121a0413)
		//} else if n % 3 == 2 {
		//	dip = ip4_t(0xd83adbe4)
		//	dport = uint16(80)
		//}
		//n++
		tcb, err := _tcp_connect(bterm, dport)
		if err != 0 {
			fmt.Printf("socket failed: %d\n", err)
		} else {
			go func() {
			sum := uint(0)
			totbytes := 0
			//_buf := make([]uint8, 64)
			_buf := make([]uint8, 1024)
			fub := &userbuf_t{}
			for {
				//time.Sleep(500*time.Millisecond)
				time.Sleep(5*time.Millisecond)
				buf := _buf
				fub.fake_init(buf)

				tcb.tcb_lock()
				l, err := tcb.uread(fub)
				tcb.tcb_unlock()
				if err != 0 {
					panic("wut?")
				}
				buf = buf[:l]
				//if len(buf) != 0 {
				//	fmt.Printf("GOT: %s\n", string(buf))
				//}
				for _, by := range buf {
					sum += uint(by)
				}
				totbytes += len(buf)
				if len(buf) != 0 {
					fmt.Printf("sum: %x (%v)\n", sum,
					    totbytes)
				}
			}
			}()
			time.Sleep(10*time.Second)
		}
		n++
	}
}

func (x *x540_t) tester5() {
	n := 0
	for {
		//mat := ip4_t(0x121a0413)
		bterm := ip4_t(0x121a0530)
		dport := uint16(31338 + n)
		//boss := ip4_t(0x9b622046)
		//dport := uint16(22)
		//if n % 3 == 1 {
		//	dip = ip4_t(0x121a0413)
		//} else if n % 3 == 2 {
		//	dip = ip4_t(0xd83adbe4)
		//	dport = uint16(80)
		//}
		//n++
		tcb, err := _tcp_connect(bterm, dport)
		if err != 0 {
			fmt.Printf("socket failed: %d\n", err)
		} else {
			go func() {
			buf := []uint8("hello from biscuit over TCP")
			fub := &userbuf_t{}
			for {
				time.Sleep(time.Second)
				fub.fake_init(buf)

				tcb.tcb_lock()
				l, err := tcb.uwrite(fub)
				tcb.tcb_unlock()
				if err != 0 {
					panic("wut?")
				}
				if l != len(buf) {
					panic("short write")
				}
				fmt.Printf("wrote %v bytes\n", len(buf))
			}
			}()
			time.Sleep(10*time.Second)
		}
		n++
	}
}

func (x *x540_t) tester6() {
	//goog := ip4_t(0xacd90444)
	obsd := ip4_t(0x818005c2)
	dport := uint16(80)
	tcb, err := _tcp_connect(obsd, dport)
	if err != 0 {
		panic("socket failed: %d")
	}

	fub := &userbuf_t{}
	getmsg := []uint8("GET / HTTP/1.0\r\n\r\n")
	fub.fake_init(getmsg)

	tcb.tcb_lock()
	l, err := tcb.uwrite(fub)
	tcb.tcb_unlock()
	if l != len(getmsg) || err != 0 {
		panic("uwrite failed")
	}

	sent := false
	for !sent {
		tcb.tcb_lock()
		if tcb.state == ESTAB {
			tcb.seg_maybe()
			sent = true
		}
		tcb.tcb_unlock()
		time.Sleep(10*time.Millisecond)
	}

	_buf := make([]uint8, 1024)
	//totbytes := 0
	//sum := 0
	for {
		//time.Sleep(500*time.Millisecond)
		time.Sleep(5*time.Millisecond)
		buf := _buf
		fub.fake_init(buf)

		tcb.tcb_lock()
		l, err := tcb.uread(fub)
		tcb.tcb_unlock()
		if err != 0 {
			panic("wut?")
		}
		buf = buf[:l]
		//totbytes += len(buf)
		//for _, c := range buf {
		//	sum += int(c)
		//}
		if len(buf) != 0 {
			//fmt.Printf("sum: %x (%d)\n", sum, totbytes)
			fmt.Printf("GOT: %s\n", string(buf))
		}
	}
}

func attach_x540t(vid, did int, t pcitag_t) {
	if unsafe.Sizeof(*rxdesc_t{}.hwdesc) != 16 ||
	   unsafe.Sizeof(*txdesc_t{}.hwdesc) != 16 {
		panic("unexpected padding")
	}

	b, d, f := breakpcitag(t)
	fmt.Printf("X540: %x %x (%d:%d:%d)\n", vid, did, b, d, f)
	if uint(f) > 1 {
		panic("virtual functions not supported")
	}

	var x x540_t
	x.init(t)

	// x540 doc 4.6.3 initialization sequence
	x._int_disable()
	x._reset()
	x._int_disable()

	// even though we disable flow control, we write 0 to FCTTV, FCRTL,
	// FCRTH, FCRTV, and  FCCFG. we program FCRTH.RTH later.
	regn := func(r x540reg_t, i int) x540reg_t {
		return r + x540reg_t(i * 4)
	}

	fcttv := x540reg_t(0x3200)
	for i := 0; i < 4; i++ {
		x.rs(regn(fcttv, i), 0)
	}
	fcrtl := x540reg_t(0x3220)
	for i := 0; i < 8; i++ {
		x.rs(regn(fcrtl, i), 0)
		x.rs(FCRTH(i), 0)
	}

	fcrtv := x540reg_t(0x32a0)
	fccfg := x540reg_t(0x3d00)
	x.rs(fcrtv, 0)
	x.rs(fccfg, 0)

	mflcn := x540reg_t(0x4294)
	rfce := uint32(1 << 3)
	son := x.rl(mflcn) & rfce != 0
	if son {
		panic("receive flow control should be off?")
	}

	// enable no snooping
	nosnoop_dis := uint32(1 << 16)
	v := x.rl(CTRL_EXT)
	if v & nosnoop_dis != 0 {
		x.log("no snoop disabled. enabling.")
		x.rs(CTRL_EXT, v &^ nosnoop_dis)
	}
	// useful for testing whether no snoop/relaxed memory ordering affects
	// buge behavior
	//ro_dis := uint32(1 << 17)
	//x.rs(CTRL_EXT, v | nosnoop_dis | ro_dis)

	x.hwlock()
	phyreset := uint16(1 << 6)
	for x._phy_read(ALARMS1) & phyreset == 0 {
	}
	//x.log("phy reset complete")
	x.hwunlock()

	// 4.6.3 says to wait for CFG_DONE, but this bit never seems to set.
	// openbsd does not check this bit.

	//x.log("manage wait")
	//cfg_done := uint32(1 << 18 + f)
	//for x.rl(EEMNGCTL) & cfg_done == 0 {
	//}
	////x.log("manageability complete")

	dmadone := uint32(1 << 3)
	for x.rl(RDRXCTL) & dmadone == 0 {
	}
	//x.log("dma engine initialized")

	// hardware reset is complete

	// RAL/RAH are big-endian
	v = x.rl(RAH(0))
	av := uint32(1 << 31)
	if v & av == 0 {
		panic("RA 0 invalid?")
	}
	mac := (uint64(v) & 0xffff) << 32
	mac |= uint64(x.rl(RAL(0)))
	for i := 0; i < 6; i++ {
		b := uint8(mac >> (8*uint(i)))
		x.mac[i] = b
	}

	// enable MSI interrupts
	msiaddrl := 0x54
	msidata := 0x5c

	maddr := 0xfee << 20
	pci_write(x.tag, msiaddrl, maddr)
	vec := msi_alloc()
	mdata := int(vec) | bsp_apic_id << 12
	pci_write(x.tag, msidata, mdata)

	msictrl := 0x50
	pv := pci_read(x.tag, msictrl, 4)
	msienable := 1 << 16
	pv |= msienable
	pci_write(x.tag, msictrl, pv)

	msimask := 0x60
	if pci_read(x.tag, msimask, 4) & 1 != 0 {
		panic("msi pci masked")
	}

	// make sure legacy PCI interrupts are disabled
	pciintdis := 1 << 10
	pv = pci_read(x.tag, 0x4, 2)
	pci_write(x.tag, 0x4, pv | pciintdis)

	// disable autoclear/automask
	x.rs(EIAC, 0)
	x.rs(EIAM, 0)
	x.rs(EIAM1, 0)
	x.rs(EIAM2, 0)

	// disable interrupt throttling
	for n := 0; n < 128; n++ {
		x.rs(EITR(n), 0)
	}

	// map all 128 rx queues to EICR bit 0 and tx queues to EICR bit 1
	for n := 0; n < 64; n++ {
		v := uint32(0x81808180)
		x.rs(IVAR(n), v)
	}

	// disable RSC; docs say RSC is enabled by default, but it isn't on my
	// card...
	for n := 0; n < 128; n++ {
		v := x.rl(RSCCTL(n))
		rscen := uint32(1 << 0)
		x.rs(RSCCTL(n), v &^ rscen)
	}

	// receive enable here
	{
		for i := 0; i < 8; i++ {
			x.rs(PFVFSPOOF(i), 0)
		}
		for i := 0; i < 256; i++ {
			x.rs(MPSAR(i), 0)
		}
		for i := 0; i < 128; i++ {
			x.rs(PFUTA(i), 0)
			x.rs(VFTA(i), 0)
			x.rs(PFVLVFB(i), 0)
		}
		// enable ethernet broadcast packets via FCTRL.BAM in order to
		// receive ARP requests.
		v := x.rl(FCTRL)
		// XXX debugging features: store bad packets and unicast
		// promiscuous
		//sbp := uint32(1 << 1)
		//upe := uint32(1 << 9)
		bam := uint32(1 << 10)
		v |= bam
		x.rs(FCTRL, v)

		v = x.rl(RXCSUM)
		ippcse := uint32(1 << 12)
		v |= ippcse
		x.rs(RXCSUM, v)

		v = x.rl(RDRXCTL)
		// HLREG0.RXCRCSTRP must match RDRXCTL.CRCSTRIP
		crcstrip := uint32(1 << 1)
		// for the two following bits, docs say: "reserved, software
		// should set this bit to 1" but the bit is initialized to 0??
		res1 := uint32(1 << 25)
		res2 := uint32(1 << 26)
		v |= crcstrip | res1 | res2
		x.rs(RDRXCTL, v)

		// bit 0 of the first word of an rx descriptor should disable
		// no snoop for packet write back when set, but my NIC seems to
		// ignore this bit! thus i disable no snoop for all rx packet
		// writebacks
		rxdatawritensen := uint32(1 << 12)
		v = x.rl(DCA_RXCTRL(0))
		v &^= rxdatawritensen
		x.rs(DCA_RXCTRL(0), v)

		// if we want to, enable jumbo frames here

		// setup rx queue 0
		pg, p_pg := x.pg_new()
		// RDBAL/TDBAL must be 128 byte aligned
		x.rs(RDBAL(0), uint32(p_pg))
		x.rs(RDBAH(0), uint32(p_pg >> 32))
		x.rs(RDLEN(0), uint32(PGSIZE))

		// packet buffers must be at least SRRCTL.BSIZEPACKET bytes,
		// header buffers must be at least SRRCTL.BSIZEHEADER bytes
		rdescsz := uint32(16)
		x.rx.ndescs = uint32(PGSIZE)/rdescsz
		x.rx.descs = make([]rxdesc_t, x.rx.ndescs)
		for i := 0; i < len(pg); i += 4 {
			_, p_bpg := x.pg_new()
			// SRRCTL.BSIZEPACKET
			dn := i / 2
			ps := uintptr(2 << 10)
			x.rx.descs[dn].init(p_bpg, 0, &pg[i])
			x.rx.descs[dn+1].init(p_bpg + ps, 0, &pg[i+2])
		}

		v = x.rl(SRRCTL(0))
		// XXX how large should a single rx descriptor's packet buffer
		// be? leave them the default for now (2K)...

		// program receive descriptor minimum threshold here

		// section 7.1.10.1's DESCTYPE values contradict the register
		// description in section 8! section 7.1.10.1 is correct.
		desctype := uint32(1 << 25)
		v |= desctype
		x.rs(SRRCTL(0), v)

		for i := range x.rx.descs {
			x.rx.descs[i].ready()
		}
		x.rs(RDH(0), 0)

		// enable this rx queue
		v = x.rl(RXDCTL(0))
		qen := uint32(1 << 25)
		v |= qen
		x.rs(RXDCTL(0), v)
		for x.rl(RXDCTL(0)) & qen == 0 {
		}
		// must enable queue via RXDCTL.ENABLE before setting RDT
		x.rs(RDT(0), x.rx.ndescs - 1)

		// enable receive
		v = x.rl(RXCTRL)
		rxen := uint32(1 << 0)
		v |= rxen
		x.rs(RXCTRL, v)
		//x.log("RX enabled!")
	}

	// transmit init
	{
		// map all per-tx-queue statistics to counter 0
		for n := 0; n < 32; n++ {
			x.rs(TQSM(n), 0)
		}

		txcrc      := uint32(1 <<  0)
		crcstrip   := uint32(1 <<  1)
		txpad      := uint32(1 << 10)
		rxlerr     := uint32(1 << 27)
		// HLREG0.RXCRCSTRP must match RDRXCTL.CRCSTRIP
		v := x.rl(HLREG0)
		v |= txcrc | crcstrip | txpad | rxlerr
		x.rs(HLREG0, v)

		nosnoop_tso := uint32(1 << 1)
		v = x.rl(DMATXCTL)
		v |= nosnoop_tso
		x.rs(DMATXCTL, v)

		// XXX fill in mask bits for TCP segment flags
		//x.rs(DTXTCPFLGL, xxx)
		//x.rs(DTXTCPFLGH, xxx)

		// XXX may want to enable relaxed ordering or DCA for tx
		//for n := 0; n < 128; n++ {
		//	x.rs(DCA_TXCTRL(n), xxx)
		//}

		// if necessary, setup IPG (inter-packet gap) here
		// ...

		x._dbc_init()

		// setup tx queues
		pg, p_pg := x.pg_new()
		// RDBAL/TDBAL must be 128 byte aligned
		x.rs(TDBAL(0), uint32(p_pg))
		x.rs(TDBAH(0), uint32(p_pg >> 32))
		x.rs(TDLEN(0), uint32(PGSIZE))

		tdescsz := uint32(16)
		ndescs := uint32(PGSIZE)/tdescsz
		x.tx.ndescs = ndescs
		x.tx.descs = make([]txdesc_t, x.tx.ndescs)
		for i := 0; i < len(pg); i += 4 {
			_, p_bpg := x.pg_new()
			dn := i / 2
			ps := uintptr(PGSIZE) / 2
			x.tx.descs[dn].init(p_bpg, ps, &pg[i])
			x.tx.descs[dn+1].init(p_bpg + ps, ps, &pg[i+2])
		}

		// disable transmit descriptor head write-back. we may want
		// this later.
		x.rs(TDWBAL(0), 0)

		// number of transmit descriptors per cacheline, as per
		// 7.2.3.4.1.
		tdcl := uint32(64/tdescsz)
		// number of internal NIC descriptor buffers 7.2.1.2
		nicdescs := uint32(40)
		pthresh := nicdescs - tdcl
		hthresh := tdcl
		wthresh := uint32(0)
		if pthresh &^ 0x7f != 0 || hthresh &^ 0x7f != 0 ||
		    wthresh &^ 0x7f != 0 {
			panic("bad pre-fetcher thresholds")
		}
		v = uint32(pthresh | hthresh << 8 | wthresh << 16)
		x.rs(TXDCTL(0), v)

		x.rs(TDT(0), 0)
		x.rs(TDH(0), 0)

		v = x.rl(DMATXCTL)
		dmatxenable := uint32(1 << 0)
		v |= dmatxenable
		x.rs(DMATXCTL, v)

		v = x.rl(TXDCTL(0))
		txenable := uint32(1 << 25)
		v |= txenable
		x.rs(TXDCTL(0), v)

		for x.rl(TXDCTL(0)) & txenable == 0 {
		}
		//x.log("TX enabled!")
	}

	// configure interrupts with throttling
	x.rs(GPIE, 0)
	// set minimum interrupt period to 1.048 ms (the largest possible
	// period). openbsd's period is 125us. EITR[1-128] are reserved for
	// MSI-X
	cnt_wdis := uint32(1 << 31)
	maxitr := uint32(0x1ff << 3)
	x.rs(EITR(0), cnt_wdis | maxitr)

	// mode clear all previous interrupts
	x.rs(EICR, ^uint32(0))

	go x.int_handler(vec)
	// unmask tx/rx queue interrupts and link change
	lsc := uint32(1 << 20)
	x.rs(EIMS, lsc | 3)

	macs := mac2str(x.mac[:])
	x.log("attached: MAC %s, rxq %v, txq %v, MSI %v, %vKB", macs,
	    x.rx.ndescs, x.tx.ndescs, vec, x.pgs << 2)
}

var numpkts int
var dropints int
var waits int
var spurs int

func (x *x540_t) rx_test() {
	prstat := func(v bool) {
		a := x.rl(SSVPC)
		b := x.rl(GPRC)
		c := x.rl(XEC)
		d := x.rl(FCCRC)
		e := x.rl(CRCERRS)
		f := x.rl(ILLERRC)
		g := x.rl(ERRBC)
		h := x.rl(PRC64)
		i := x.rl(PRC127)
		j := x.rl(QPRC(0))
		k := x.rl(QPRDC(0))
		if v {
			fmt.Println("  RX stats: ", a, b, c, d, e, f, g,
			    h, i, j, k)
		}
	}
	prstat(false)

	i := 0
	for {
		i++
	//for i := 0; i < 3; i++ {
		<-time.After(time.Second)
		tail := x.rl(RDT(0))
		rdesc := &x.rx.descs[tail]
		rdesc.ready()
		tail++
		if tail == x.rx.ndescs {
			tail = 0
		}

		x.rs(RDT(0), tail)
		for !rdesc.rxdone() {
		}
		eop := uint64(1 << 1)
		if rdesc.hwdesc.p_hdr & eop == 0 {
			panic("EOP not set")
		}
		if x.rl(RDH(0)) != tail {
			panic("wtf")
		}

		hl := rdesc.hdrlen()
		pl := rdesc.pktlen()
		if pl > 1 << 11 {
			panic("expected packet len")
		}
		fmt.Printf("packet %v: plen: %v, hdrlen: %v, hdr: ", i, pl, hl)
		b := dmaplen(uintptr(rdesc.p_pbuf), int(rdesc.pktlen()))
		for _, c := range b[:hl] {
			fmt.Printf("%0.2x ", c)
		}
		fmt.Printf("\n")
	}
}

func (x *x540_t) tx_test() {
	for i := range x.mac {
		txdata.macsrc[i] = x.mac[i]
	}
	// statistic regs auto-clear on read; clear them all
	prstat := func(v bool) {
		a := x.rl(SSVPC)
		b := x.rl(GPTC)
		c := x.rl(TXDGPC)
		d := x.rl(TPT)
		var e uint32
		for i := 0; i < 16; i++ {
			e += x.rl(QPTC(i))
		}
		f := x.rl(PTC64)
		g := x.rl(PTC127)
		h := x.rl(MSPDC)
		i := x.rl(XEC)
		j := x.rl(BPTC)
		k := x.rl(QPTC_L(0))
		l := x.rl(FCCRC)
		m := x.rl(B2OSPC)
		n := x.rl(B2OGPRC)
		o := x.rl(O2BGPTC)
		p := x.rl(O2BSPC)
		q := x.rl(CRCERRS)
		r := x.rl(ILLERRC)
		s := x.rl(ERRBC)
		if v {
			fmt.Println("  TX stats: ", a, b, c, d, e, f, g,
			    h, i, j, k, l, m, n, o, p, q, r, s)
		}
	}
	prstat(false)

	for i := 0; i < 3; i++ {
		//x.log("PACKET %v", i)
		// setup tx descriptor
		tail := x.rl(TDT(0))
		tdesc := &x.tx.descs[tail]
		const paylen = unsafe.Sizeof(txdata_t{})
		bytes := (*[paylen]uint8)(unsafe.Pointer(&txdata))[:]
		sgbuf := [][]uint8{bytes}
		tdesc.mkraw(sgbuf[:], len(bytes))
		tail++
		if tail == x.tx.ndescs {
			tail = 0
		}

		//prstat(true)
		//x.log("bump tail: %v", tail)
		x.rs(TDT(0), tail)
		for {
			if tdesc.txdone() {
				break
			}
			//prstat(true)
			//<-time.After(10*time.Millisecond)
		}
		x.log("transmitted?")
		if x.rl(TDH(0)) != tail {
			panic("TDT/TDH mismatch")
		}
		prstat(true)
	}
}

type txdata_t struct {
	macdst	[6]uint8
	macsrc	[6]uint8
	etype	[2]uint8
	payload	[28]uint8
}

// fake ARP packet
var txdata = txdata_t {
	macdst: [6]uint8{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	macsrc: [6]uint8{0xa0, 0x36, 0x9f, 0xb3, 0xc3, 0x08},
	etype: [2]uint8{0x08, 0x06},
	payload: [28]uint8{0x00, 0x01, 0x08, 0x00, 0x06, 0x04, 0x00, 0x02, 0x00, 0x13, 0x72, 0xb6, 0x7b, 0x42, 0x12, 0x1a, 0x05, 0x30, 0xa0, 0x36, 0x9f, 0xb3, 0xc3, 0x08, 0x12, 0x1a, 0x05, 0x31},
}

func (x *x540_t) _dbc_init() {
	// dbc=off, vt=off (section 4.6.11.3.4)
	rxpbsize := uint32(0x180 << 10)
	x.rs(RXPBSIZE(0), rxpbsize)
	for n := 1; n < 8; n++ {
		x.rs(RXPBSIZE(n), 0)
	}
	// 4.6.3.2 "FCRTH.RTH must be set even if flow control is disabled"
	x.rs(FCRTH(0), x.rl(RXPBSIZE(0)) - 0x6000)

	txpbsize := uint32(0xa0 << 10)
	x.rs(TXPBSIZE(0), txpbsize)
	for n := 1; n < 8; n++ {
		x.rs(TXPBSIZE(n), 0)
	}
	txpbthresh := uint32(0xa0)
	x.rs(TXPBTHRESH(0), txpbthresh)
	for n := 1; n < 8; n++ {
		x.rs(TXPBTHRESH(n), 0)
	}

	v := x.rl(MRQC)
	mrqe := uint32(0xf)
	v &^= mrqe
	x.rs(MRQC, v)

	// RTTDCS.ARBDIS must be 0 before programming MTQC
	arbdis := uint32(1 << 6)
	v = x.rl(RTTDCS)
	v |= arbdis
	x.rs(RTTDCS, v)

	v = x.rl(MTQC)
	vtdbc := uint32(0xf)
	v &^= vtdbc
	x.rs(MTQC, v)

	v = x.rl(RTTDCS)
	v &^= arbdis
	x.rs(RTTDCS, v)

	vten := uint32(1 << 0)
	v = x.rl(PFVTCTL)
	v &^= vten
	x.rs(PFVTCTL, v)

	v = x.rl(PFQDE)
	dropen := uint32(1 << 0)
	v &^= dropen
	x.rs(PFQDE, v)

	x.rs(RTRUP2TC, 0)
	x.rs(RTTUP2TC, 0)

	x.rs(DTXMXSZRQ, 0xfff)

	v = x.rl(SECTXMINIFG)
	// docs contradict: says use both 0x10 and 0x1f when in non DBC
	// mode?
	mrkrinstert := uint32(0x10 << 8)
	mrkmask := uint32(((1 << 11) - 1) << 8)
	v &^= mrkmask
	v |= mrkrinstert
	x.rs(SECTXMINIFG, v)
	v = x.rl(MFLCN)
	rpfcm := uint32(1 << 2)
	rpfcemask := uint32(0xff << 4)
	v &^= rpfcm | rpfcemask
	// XXX do we really need to enable flow control as 4.6.11.3.4
	// claims?
	//rfce := uint32(1 << 3)
	//v |= rfce
	x.rs(MFLCN, v)
	//x.rs(FCCFG,

	for n := 0; n < 128; n++ {
		x.rs(RTTDQSEL, uint32(n))
		x.rs(RTTDT1C, 0)
	}
	for n := 0; n < 8; n++ {
		x.rs(RTTDT2C(n), 0)
	}
	for n := 0; n < 8; n++ {
		x.rs(RTTPT2C(n), 0)
	}
	for n := 0; n < 8; n++ {
		x.rs(RTRPT4C(n), 0)
	}

	v = x.rl(RTTDCS)
	tdpac  := uint32(1 <<  0)
	vmpac  := uint32(1 <<  1)
	tdrm   := uint32(1 <<  4)
	bdpm   := uint32(1 << 22)
	bpbfsm := uint32(1 << 23)
	v &^= tdpac | vmpac | tdrm
	v |= bdpm | bpbfsm
	x.rs(RTTDCS, v)

	v = x.rl(RTTPCS)
	tppac  := uint32(1 << 5)
	tprm   := uint32(1 << 8)
	arbd   := uint32(0x224 << 22)
	arbmask := uint32(((1 << 10) - 1) << 22)
	v &^= tppac | tprm | arbmask
	v |= arbd
	x.rs(RTTPCS, v)

	v = x.rl(RTRPCS)
	rrm  := uint32(1 << 1)
	rac  := uint32(1 << 2)
	v &^= rrm | rac
	x.rs(RTRPCS, v)
}
