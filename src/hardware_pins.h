#ifndef HARDWARE_PINS_H
#define HARDWARE_PINS_H

// --- Power Outputs Configuration ---
// GPIO Pins for each power output
#define POWER_DC1_PIN 13
#define POWER_DC2_PIN 12
#define POWER_DC3_PIN 14
#define POWER_DC4_PIN 27
#define POWER_DC5_PIN 26
#define POWER_USBC12_PIN 19
#define POWER_USB345_PIN 18

// --- Adjustable DC Converter Configuration ---
#define ADJUSTABLE_CONVERTER_PIN 25
// The voltage at 100% PWM duty cycle. This is a hardware property of the converter circuit.
#define ADJUSTABLE_CONVERTER_MAX_VOLTAGE 15.0 

// --- Dew Heater Configuration ---
#define DEW_HEATER_1_PIN 33
#define DEW_HEATER_2_PIN 32

// --- I2C Configuration ---
#define I2C_SDA 21
#define I2C_SCL 22

// --- Sensor Addresses ---
#define INA219_ADDR 0x40
#define SHT40_ADDR  0x44

// --- OneWire Configuration ---
#define ONE_WIRE_BUS 23

#endif // HARDWARE_PINS_H
